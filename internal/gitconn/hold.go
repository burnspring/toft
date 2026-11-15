package gitconn

import (
	"context"
	"fmt"

	"toft/internal/database"
)

// The `hold` method: the connection's stored credential, used as it is.
//
// GitLab's group token and GitHub's personal access token share this path, and
// **nothing below `Credential` knows which happened** — that is D6's claim, and
// the reason it is worth stating is that a `mint` path lands beside this one
// without a shared abstraction above either. Two functions, one struct, and a
// table test that walks all three combinations through the same `Explain`.

// connectionCredential resolves an app that points at a connection.
//
// The row is read **live on every call**. The resolver caches no `enabled`,
// which is what makes a disable take effect on the next clone and the next poll
// rather than on the next restart — and it is why "off means now" survives a
// feature whose whole point is a credential shared by an estate.
func (r *Resolver) connectionCredential(ctx context.Context, app *database.App) (Credential, error) {
	conn, err := r.db.GetGitConnection(app.GitConnectionID)
	if err != nil {
		return Credential{Source: SourceNone}, err
	}
	cred := Credential{Source: SourceConnection, Connection: conn.DisplayName}

	// Checked here as well as at the write, so an app row that already points
	// a connection at a foreign host fails closed instead of sending it the
	// credential.
	if why := HostRefusal(conn, app.RepoURL); why != "" {
		return cred, &Unreadable{
			Cred:    cred,
			Summary: why + " Point the app at a repository on it, or detach the connection.",
			Err:     fmt.Errorf("repository host does not match connection %s", conn.ID),
		}
	}

	if !conn.Enabled {
		// **Not an Unreadable**: the credential is perfectly readable and this
		// install has decided not to use it. It is still an auth failure to the
		// app's owner, and it still has to name the connection rather than a
		// field on their settings page.
		return cred, &Unreadable{
			Cred: cred,
			Summary: fmt.Sprintf("%s is deployed through the %s connection, which an administrator "+
				"has disabled. Nothing running has stopped; the next deploy needs it turned back on "+
				"in Admin → Sources.", app.RepoURL, conn.DisplayName),
			Err: fmt.Errorf("connection %s is disabled", conn.ID),
		}
	}

	switch conn.AuthMethod {
	case database.GitAuthApp:
		// Minted per clone and **narrowed to the repository being cloned**, so
		// a credential lifted off a build host in the seconds it exists reaches
		// one repository for one hour. Nothing below this struct knows that
		// happened.
		token, err := r.installationToken(ctx, conn, app.RepoURL)
		if err != nil {
			return cred, &Unreadable{
				Cred: cred,
				Summary: fmt.Sprintf("%s could not be cloned with the %s connection — an "+
					"administrator needs to check it in Admin → Sources.", app.RepoURL, conn.DisplayName),
				Err: err,
			}
		}
		cred.Token = token
		return cred, nil
	case database.GitAuthToken:
		token, err := r.enc.DecryptContext(ctx, conn.EncryptedCloneSecret())
		if err != nil {
			return cred, &Unreadable{
				Cred: cred,
				Summary: fmt.Sprintf("The %s connection's credential could not be read — an "+
					"administrator needs to re-enter it in Admin → Sources.", conn.DisplayName),
				Err: err,
			}
		}
		cred.Token = token
		return cred, nil
	default:
		// A method this build does not implement is refused by name rather than
		// falling back to the app's own token. Silently cloning with a different
		// credential than the one the row names is the class of behaviour this
		// package exists to remove.
		return cred, &Unreadable{
			Cred: cred,
			Summary: fmt.Sprintf("The %s connection authenticates in a way this install cannot use "+
				"(%s). An administrator needs to look at it in Admin → Sources.",
				conn.DisplayName, conn.AuthMethod),
			Err: fmt.Errorf("unsupported auth method %q on connection %s", conn.AuthMethod, conn.ID),
		}
	}
}

// Connection reads one row, for a caller that needs to check coverage or name
// the connection in a refusal. It is not a credential and returns none.
func (r *Resolver) Connection(id string) (*database.GitConnection, error) {
	return r.db.GetGitConnection(id)
}
