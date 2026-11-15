package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"toft/internal/database"
	"toft/internal/password"
)

// `toft local-admin` — the way back into an install nobody can sign in to.
//
// Everything else about identity is administered through the web UI, on purpose.
// This is the exception, and the reason is the shape of the failure it repairs:
// the IdP is down, or its client secret was rotated, or the last administrator's
// account was locked. In every one of those cases there is no session to
// authenticate an API call with, so the only credential left that means anything
// is having a shell on the host.
//
// That is also why local sign-in cannot be re-enabled through the API — see
// updateLocalProvider. Recovery requires the machine; a stolen admin cookie is
// not enough.
//
// The command talks to Postgres directly and never to the running server, so it
// works whether or not the platform is up.

const localAdminUsage = `Usage: toft local-admin <command>

Manage the built-in local administrator — the account that exists so an install
with no identity provider (or a broken one) still has a way in.

Commands:
  status              Report whether local sign-in is on, and the account's state
  enable [flags]      Switch local sign-in on and set the administrator's password
  disable             Switch local sign-in off

Flags for enable:
  --password <value>  Use this password instead of generating one. Must be at
                      least %d characters. Prefer the generated one: it is
                      stronger, and it is not in your shell history.

The account is "%s" (%s). Reads DATABASE_URL from the environment or .env,
like the server does.
`

// runLocalAdmin dispatches the subcommand. Returns an error for main to report;
// it never calls os.Exit itself, so the deferred database close always runs.
func runLocalAdmin(args []string) error {
	if len(args) == 0 {
		fmt.Printf(localAdminUsage, password.MinLength, database.BuiltinAdminLogin, database.BuiltinAdminEmail)
		return nil
	}

	db, err := openDatabaseForCLI()
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "status":
		return localAdminStatus(db)
	case "enable":
		fs := flag.NewFlagSet("local-admin enable", flag.ContinueOnError)
		chosen := fs.String("password", "", "password to set (default: generate one)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return localAdminEnable(db, *chosen)
	case "disable":
		return localAdminDisable(db)
	case "help", "-h", "--help":
		fmt.Printf(localAdminUsage, password.MinLength, database.BuiltinAdminLogin, database.BuiltinAdminEmail)
		return nil
	default:
		return fmt.Errorf("unknown command %q — run `toft local-admin help`", args[0])
	}
}

// openDatabaseForCLI connects and migrates exactly as the server does.
//
// Deliberately the same code path: a recovery tool that talked to the schema
// differently from the binary it is recovering would be one more thing that can
// be wrong at the worst possible moment. It also means the command works on a
// database that has never been migrated — `toft local-admin enable` on a
// fresh install is a valid first act.
func openDatabaseForCLI() (*database.DB, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set — run this where the server runs, or point it at the platform database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := database.Open(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	return db, nil
}

func localAdminStatus(db *database.DB) error {
	provider, err := db.GetAuthProvider(database.LocalProviderID)
	if err != nil {
		return fmt.Errorf("reading the local provider: %w", err)
	}
	user, err := db.GetUser(database.BuiltinAdminID)
	if err != nil {
		return fmt.Errorf("reading the built-in administrator: %w", err)
	}
	if user == nil {
		return fmt.Errorf("the built-in administrator is missing from this database — it is created by migration 21")
	}
	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil {
		return fmt.Errorf("reading the stored password: %w", err)
	}

	state := "off — the login page does not offer it"
	if provider.Enabled {
		state = "on — the login page offers it"
	}
	fmt.Printf("Local sign-in:  %s\n", state)
	fmt.Printf("Account:        %s (%s)\n", user.Login, user.Email)
	fmt.Printf("Role:           %s\n", user.SystemRole)

	switch {
	case user.Locked():
		fmt.Printf("Account state:  LOCKED — `enable` unlocks it\n")
	default:
		fmt.Printf("Account state:  active\n")
	}

	switch {
	case cred == nil:
		fmt.Printf("Password:       none stored — run `toft local-admin enable`\n")
	case cred.MustChange:
		fmt.Printf("Password:       must be changed at next sign-in (no session is issued until it is)\n")
	default:
		fmt.Printf("Password:       set, changed %s\n", cred.UpdatedAt.Format(time.RFC3339))
	}
	if cred != nil && cred.Throttled(time.Now().UTC()) {
		fmt.Printf("Lockout:        too many failed attempts — attempts resume %s\n",
			cred.LockedUntil.Format(time.RFC3339))
	}

	if isDefault, err := db.BuiltinAdminHasDefaultPassword(); err == nil && isDefault {
		fmt.Printf("\nThis account still has the published default password (%q). It cannot mint a\n"+
			"session — signing in with it only offers the chance to replace it — but the\n"+
			"first person to reach the login page gets to choose the new one. Set one now\n"+
			"with `toft local-admin enable`.\n", password.Default)
	}
	return nil
}

// localAdminEnable is the whole recovery path in one command: a password, an
// unlocked account, and a door that is open.
//
// All three, because the operator running this has already established that they
// cannot get in, and making them discover the remaining two problems one failed
// login at a time would be a poor way to spend an outage.
func localAdminEnable(db *database.DB, chosen string) error {
	pw := chosen
	generated := chosen == ""
	if generated {
		var err error
		if pw, err = password.Generate(); err != nil {
			return err
		}
	} else if err := password.Validate(pw); err != nil {
		return err
	}

	hash, err := password.Hash(pw)
	if err != nil {
		return err
	}

	audit := database.AuditEntry{
		ActorType: database.PrincipalTypeOperator,
		ActorID:   "cli",
	}
	// A generated password is printed to this terminal and therefore to a
	// scrollback buffer, a screen share or a ticket. It gets the same treatment as
	// the published default: usable once, to choose a real one.
	if err := db.SetPassword(database.BuiltinAdminID, hash, generated, audit); err != nil {
		return fmt.Errorf("setting the password: %w", err)
	}
	if err := db.UnlockUserForRecovery(database.BuiltinAdminID, audit); err != nil {
		return fmt.Errorf("unlocking the account: %w", err)
	}
	if err := db.SetAuthProviderEnabled(database.LocalProviderID, true, audit); err != nil {
		return fmt.Errorf("enabling local sign-in: %w", err)
	}

	fmt.Printf("Local sign-in is on. The login page now offers a username and password form.\n\n")
	fmt.Printf("  Username:  %s\n", database.BuiltinAdminLogin)
	if generated {
		fmt.Printf("  Password:  %s\n\n", pw)
		fmt.Printf("This password is shown once and is not stored anywhere you can read it back.\n")
		fmt.Printf("It must be changed at first sign-in — no session is issued until it is.\n")
	} else {
		fmt.Printf("  Password:  (the one you passed)\n\n")
		fmt.Printf("Every existing session for this account has been revoked.\n")
	}
	fmt.Printf("\nSwitch this off again from Admin → Authentication once you can sign in\n" +
		"through your identity provider.\n")
	return nil
}

func localAdminDisable(db *database.DB) error {
	// No "is there another way in" guard here, unlike the API. The operator has a
	// shell, so `enable` is always available to them — the guard exists to stop an
	// administrator locking the install out of its own UI, which is not a mistake
	// this command can make irrecoverably.
	if err := db.SetAuthProviderEnabled(database.LocalProviderID, false, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator,
		ActorID:   "cli",
	}); err != nil {
		return fmt.Errorf("disabling local sign-in: %w", err)
	}
	fmt.Printf("Local sign-in is off. The login page no longer offers it.\n")
	fmt.Printf("Re-enable it with `toft local-admin enable`.\n")
	return nil
}
