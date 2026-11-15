package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"toft/internal/advisory"
	"toft/internal/agent"
	"toft/internal/announce"
	"toft/internal/api"
	"toft/internal/applog"
	"toft/internal/appout"
	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/autodeploy"
	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/depsweep"
	"toft/internal/dormancy"
	"toft/internal/entitlements"
	"toft/internal/events"
	"toft/internal/gallery"
	"toft/internal/gitconn"
	"toft/internal/health"
	"toft/internal/hoststore"
	"toft/internal/idle"
	"toft/internal/jobs"
	"toft/internal/kube"
	"toft/internal/mail"
	"toft/internal/obs"
	"toft/internal/ports"
	"toft/internal/report"
	"toft/internal/resource"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/slack"
	"toft/internal/spa"
	"toft/internal/staticserve"
	"toft/internal/supervisor"
	"toft/internal/telemetry"
	"toft/internal/version"

	// The zone database, compiled in, so a schedule's CRON_TZ resolves the
	// same on a slim image as on a developer box (internal/schedule).
	_ "time/tzdata"
)

// auditJSON renders an audit row's before/after. The columns are jsonb, so a
// bare sentence is not a value they can hold.
func auditJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// drainTimeout bounds how long a shutdown waits for in-flight app requests.
// It has to exceed a normal page load and stay under whatever the supervisor
// allows before it sends SIGKILL — 30s is comfortably inside both ECS's and
// Kubernetes' defaults.
const drainTimeout = 30 * time.Second

// telemetryDrainTimeout bounds the last flush of spans and metrics on the way
// out. Short, and deliberately so: this is the one subsystem whose failure must
// never be a reason the process will not exit.
const telemetryDrainTimeout = 5 * time.Second

// platformNarrator adapts the platform log sink to the runtime seam.
//
// The seam is an interface declared in internal/runtime because
// internal/database imports that package, so a backend that reached the store
// directly would be an import cycle — the same reason Repairer is an interface
// there, pointing the other way. This is the whole of the adapter: two methods,
// neither of which can fail.
type platformNarrator struct{ sink *applog.Sink }

func (n platformNarrator) Starting(appID, workloadName string) {
	n.sink.StartOf(appID, workloadName)
}

func (n platformNarrator) Say(appID, workloadName, stream, line string) {
	// No deployment id: a WorkloadSpec does not carry one. workload_name holds
	// the release, which is what distinguishes one start's narration from the
	// next, and it is why that column exists.
	n.sink.For(appID, workloadName, "").Say(stream, line)
}

func main() {
	godotenv.Load()

	// Subcommands, dispatched before anything is started. `local-admin` exists to
	// repair an install that cannot be signed into, so it must not depend on the
	// server it is repairing — it talks to Postgres and exits.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "local-admin":
			if err := runLocalAdmin(os.Args[2:]); err != nil {
				log.Fatalf("local-admin: %v", err)
			}
			return
		case "advisory-import":
			// The advisory corpus for an install with no outbound HTTPS, which
			// is the customer this product is for rather than an edge case. It
			// talks to Postgres and exits, like local-admin.
			if err := runAdvisoryImport(os.Args[2:]); err != nil {
				log.Fatalf("advisory-import: %v", err)
			}
			return
		case "rotate-key":
			// The second half of a key rotation: the swap is a restart, this
			// is the sweep that lets the old key be dropped. Talks to Postgres
			// and exits, like the two above.
			if err := runRotateKey(os.Args[2:]); err != nil {
				log.Fatalf("rotate-key: %v", err)
			}
			return
		case "openapi":
			// The same question as `routes`, in the form a scanner reads.
			// See cli_openapi.go.
			if err := runOpenAPI(os.Args[2:]); err != nil {
				log.Fatalf("openapi: %v", err)
			}
			return
		case "routes":
			// Neither a database nor a server: the router can describe itself.
			// See cli_routes.go for who asks and why.
			if err := runRoutes(os.Args[2:]); err != nil {
				log.Fatalf("routes: %v", err)
			}
			return
		case staticserve.CommandName:
			// A static app's workload. It is *this* binary because a file
			// server that is already on the box adds nothing to any install —
			// no npx fetch, no generated server.js, no Go toolchain — and
			// because the isolated launcher runs the same subcommand of the
			// runner binary, so one implementation serves both modes.
			//
			// Dispatched here, above config.Load, because it needs no database,
			// no encryption key and no configuration: a release directory and a
			// port are the whole of its input.
			if err := staticserve.Serve(os.Args[2:]); err != nil {
				log.Fatalf("%s: %v", staticserve.CommandName, err)
			}
			return
		case agent.Command:
			// The shared-mode agent (internal/agent): the process that places
			// and supervises shared workloads and outlives the platform that
			// started it. Above config.Load for the runner's reason — its whole
			// input is its directory, and its environment is the allowlisted
			// base the platform started it with.
			if err := agent.Main(os.Args[2:]); err != nil {
				log.Fatalf("%s: %v", agent.Command, err)
			}
			return
		case agent.ShimCommand:
			// A namespace-tier workload's first process: PID 1 of its own
			// namespaces, building its view of the machine before it becomes
			// the runner (internal/agent/shim_linux.go). Started by the agent
			// alone.
			if err := agent.ShimMain(); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", agent.ShimCommand, err)
				os.Exit(1)
			}
			return
		case runtime.HostRunnerCommand:
			// A job run on this machine (ADR §106). The same runner the
			// runner images carry, out of this binary, so the contract version
			// a host run speaks can never differ from the platform's that
			// started it. Above config.Load for staticserve's reason: its
			// whole input is the environment the host backend gave it.
			// The runner's own diagnostics in the platform's voice, as the
			// runner image marks them (cmd/toft-runner): an unmarked line reads
			// as the app's.
			log.SetOutput(runner.MarkedWriter(os.Stderr, runner.LogSourcePlatform))
			// Its output is a FIFO the agent reads (internal/agent). An agent
			// that died and has not yet been replaced leaves a write to it with
			// no reader, and SIGPIPE would take the app down with the agent;
			// ignored, the line is lost and the app keeps serving.
			signal.Ignore(syscall.SIGPIPE)
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			opts := runner.OptionsFromEnv()
			if opts.CallbackURL == "" {
				log.Fatalf("%s: no callback URL in the environment — started by Toft, not by hand",
					runtime.HostRunnerCommand)
			}
			err := runner.Run(ctx, opts)
			stop()
			if err != nil {
				log.Printf("%v", err)
				// The app's own end is the runner's, so the agent reaping the
				// runner reads what the app did (RUN.12, RUN.36): its exit
				// code, or the signal that killed it.
				var exit runner.AppExit
				if errors.As(err, &exit) {
					runner.ExitAs(exit)
				}
				os.Exit(1)
			}
			return
		default:
			log.Fatalf("unknown command %q — the subcommands are `local-admin`, "+
				"`advisory-import`, `rotate-key`, `routes`, `%s`, `%s` and `%s`; run with no arguments to start the server",
				os.Args[1], staticserve.CommandName, runtime.HostRunnerCommand, agent.Command)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Before anything else logs. This re-points the standard `log` package too,
	// so every line below arrives in one stream and one format whether it was
	// written with `log.Printf` or `slog`.
	logLevel := obs.Init(cfg.LogFormat, cfg.LogLevel)

	// Database. Open migrates the schema to the latest version before returning,
	// so the binary is the only thing that ever touches the schema.
	gateLow, gateHigh, err := runtime.ParseGIDRange(cfg.SharedStorageGIDRange)
	if err != nil {
		fatalf("SHARED_STORAGE_GID_RANGE: %v", err)
	}
	dbOpts := []database.Option{database.WithBootstrapAdminPassword(cfg.BootstrapAdminPassword)}
	if gateLow != 0 {
		dbOpts = append(dbOpts, database.WithGateGIDRange(gateLow, gateHigh))
	}
	acctLow, acctHigh, err := runtime.ParseAccountIDRange(cfg.AppAccountIDRange)
	if err != nil {
		fatalf("APP_ACCOUNT_ID_RANGE: %v", err)
	}
	if acctLow != 0 {
		dbOpts = append(dbOpts, database.WithAccountIDRange(acctLow, acctHigh))
	}
	// Both ranges are gids — a per-app group takes its account's number — so
	// they must not meet, defaults or not.
	{
		gl, gh := gateLow, gateHigh
		if gl == 0 {
			gl, gh = database.DefaultGateGIDLow, database.DefaultGateGIDHigh
		}
		al, ah := acctLow, acctHigh
		if al == 0 {
			al, ah = database.DefaultAccountIDLow, database.DefaultAccountIDHigh
		}
		if al <= gh && gl <= ah {
			fatalf("APP_ACCOUNT_ID_RANGE %d-%d overlaps SHARED_STORAGE_GID_RANGE %d-%d: a per-app group and a storage gate would share a gid", al, ah, gl, gh)
		}
	}
	dbCtx, cancelDB := context.WithTimeout(context.Background(), 60*time.Second)
	db, err := database.Open(dbCtx, cfg.DatabaseURL, dbOpts...)
	cancelDB()
	if err != nil {
		fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// The operating envelope: this process's numbers, with the administrator's
	// dials resolved over them at each read. Constructed once and handed down;
	// never resolved into values here, because a value resolved at process
	// start is a value that can only change on a restart.
	dials := config.NewResolver(cfg, db)

	// The one dial that takes effect without waiting for anything to start.
	// Applied here rather than left at the environment's, because a restart
	// must not quietly undo a level an administrator set: the row still wins,
	// and it wins from the first line this process writes after the database is
	// readable.
	logLevel.Set(dials.Envelope().LogLevel)

	// Job limits became an administrator's (migration 00076); a job whose
	// memory was the install's app default keeps it, resolved here because
	// that default may be the environment's.
	{
		env := dials.Envelope()
		appDefault := env.AppMemoryLimitMB
		if appDefault <= 0 {
			appDefault = 512 // what an undeclared job ran at with an uncapped install
		}
		if err := db.SeedJobMemoryOverrides(appDefault, env.JobMemoryLimitMB); err != nil {
			log.Printf("seeding job memory overrides: %v", err)
		}
	}

	// Encryption. One key writes; the ring reads. A key in ENCRYPTION_KEY_OLD
	// is retired — kept so records written under it still open, never used for
	// a new write — which is what makes rotating the key a restart instead of
	// an uninterruptible pass over the estate.
	//
	// ENCRYPTION_KEY_URI moves the encrypt key into a KMS. Presence decides, and
	// **resolution is here, once**: a URI that names an unreachable KMS is fatal
	// now rather than a platform that quietly writes under a local key an hour
	// from now. It is built after the database because the wrapped data key
	// lives there.
	enc, err := crypto.New(context.Background(), cfg.EncryptionKey, cfg.EncryptionKeyOld, crypto.Options{
		URI:  cfg.EncryptionKeyURI,
		DEKs: db,
	})
	if err != nil {
		fatalf("Failed to initialize encryption: %v", err)
	}
	if enc.Backend() == crypto.BackendManaged {
		log.Printf("encryption: managed by %s, writing under data key %s, reading %d keys (%s)",
			enc.KEKURI(), enc.EncryptKeyID(), len(enc.KeyIDs()), strings.Join(enc.KeyIDs(), ", "))
	} else if len(cfg.EncryptionKeyOld) > 0 {
		log.Printf("encryption: writing under key %s, reading %d keys (%s)",
			enc.EncryptKeyID(), len(enc.KeyIDs()), strings.Join(enc.KeyIDs(), ", "))
	}
	reportUnreadableSecrets(enc, db)

	// Telemetry. Unconfigured, this returns a no-op shutdown having started no
	// goroutine, registered no provider and opened no socket — the capability is
	// absent rather than dormant, which is the same rule the artifact store, the
	// isolated backend and every notification channel follow.
	//
	// It is started after the database because the resource carries
	// `deployment.environment.name` from the environment_tag *setting* (ADR §14),
	// and before everything else because a reconcile, a backfill and a build
	// queue sweep all happen below and are worth seeing.
	telemetryDoc, err := db.GetTelemetrySettings()
	if err != nil {
		// Not fatal. An install whose settings table cannot be read has larger
		// problems than telemetry, and falling back to the environment is the
		// behaviour every install had before the document existed.
		log.Printf("telemetry: reading settings, falling back to the environment: %v", err)
		telemetryDoc = database.DefaultTelemetrySettings()
	}
	telemetryHeaders, err := decryptTelemetryHeaders(enc, telemetryDoc)
	if err != nil {
		// A credential that will not decrypt is a rotated ENCRYPTION_KEY, in
		// practice. Exporting without it would authenticate as nobody and fail
		// at the collector, which is a confusing way to learn that; saying so
		// here and carrying on is clearer, and the admin page can fix it.
		log.Printf("telemetry: the stored collector credential could not be read (%v) — "+
			"re-enter it on Admin → Telemetry", err)
		telemetryHeaders = ""
	}
	stopTelemetry, err := telemetry.Init(context.Background(),
		telemetry.Resolve(cfg.Telemetry, telemetryDoc, telemetryHeaders),
		telemetryOptions(cfg, db))
	if err != nil {
		// Fatal rather than a warning, deliberately. Every reachable error here
		// is a misconfiguration the operator can fix in the place they already
		// edit — and a platform that silently exported nothing after being told
		// to export is the failure this whole subsystem exists to prevent.
		fatalf("Failed to start telemetry: %v", err)
	}
	logTelemetry(cfg, telemetry.Resolve(cfg.Telemetry, telemetryDoc, telemetryHeaders))
	// The observable gauges read state this process owns but internal/telemetry
	// does not. Registered here, once, and read on the export interval rather
	// than on any request path.
	telemetry.ObserveDBPool(func() telemetry.PoolStats {
		s := db.PoolStats()
		return telemetry.PoolStats{
			InUse: int64(s.InUse), Idle: int64(s.Idle),
			Open: int64(s.OpenConnections), Max: int64(s.MaxOpenConnections),
			Waits: s.WaitCount, WaitSeconds: s.WaitDuration.Seconds(),
		}
	})
	telemetry.ObserveAppsByStatus(func() map[string]int64 {
		// A failed read observes nothing rather than observing zero. Zero is a
		// claim ("there are no running apps"); absent is the truth ("nothing is
		// reporting"), and an alert on "running apps dropped to 0" must not fire
		// because a query timed out.
		counts, err := db.CountAppsByStatus()
		if err != nil {
			log.Printf("telemetry: counting apps by status: %v", err)
			return nil
		}
		return counts
	})

	// Port allocation for deployed apps. It belongs to the execution backend,
	// not to the builder: nothing that asks for a workload picks its address,
	// and a backend that places workloads elsewhere has no use for ports.
	allocator := ports.New(cfg.PortRangeStart, cfg.PortRangeEnd, db.UsedPorts)

	// The artifact store is what makes a release outlive the machine that
	// built it. Unconfigured means the capability is not offered: releases
	// stay local directories and no object-store code path activates.
	artifacts, err := openArtifactStore(cfg)
	if err != nil {
		fatalf("Failed to open the artifact store: %v", err)
	}
	// Wrapped so every store operation is timed. A no-op when telemetry is off,
	// and named by backend rather than by bucket: which bucket an install uses
	// is not a bounded label, and the question a graph answers is "is S3 slow",
	// not "is this bucket slow".
	artifacts = artifact.Instrumented(artifacts, cfg.ArtifactStore)
	if artifacts == nil {
		log.Printf("No artifact store configured (ARTIFACT_STORE unset). Releases live only " +
			"in APPS_DIR — on a host whose disk does not survive replacement, a restart " +
			"loses every release.")
	} else {
		log.Printf("Artifact store: %s", cfg.ArtifactStore)
	}

	// The resolver is the one place that knows a release may be something
	// other than a directory already on this disk. Restore-on-boot happens
	// here, so neither the reconciler nor the backend has to know a fetch
	// was involved.
	resolver := runtime.ReleaseResolver(builder.ArtifactResolver{
		Store:   artifacts,
		AppsDir: cfg.AppsDir,
	})

	// The platform's own voice about one app, kept where its owner can read it
	// after the workload is gone.
	//
	// Constructed here because everything that narrates is constructed below
	// it, and closed on the way out so the last thing said before a restart is
	// on disk rather than in a buffer — which is the difference from
	// usage.Recorder, whose loss on shutdown is an acceptable rounding error
	// for a counter and is not one for this.
	platformLog := applog.New(db)
	defer platformLog.Close()

	// The store signer, behind `toft store put`.
	//
	// Not derived from the storage backend above, because they are two
	// different capabilities: binding a store to an app needs no AWS permission
	// at all, and signing into one needs PutObject/GetObject/ListBucket on its
	// prefix. STORE_UPLOADS is the operator saying they have written that
	// policy. Absent, the /api/v1/store routes 404.
	//
	// Declared as the interface, never as the backend: a nil *S3Signer stored
	// in an interface field is not a nil interface, and the API server's
	// "absent, not dormant" check would read it as present.
	var storeSigner artifact.Signer
	if cfg.StoreUploads {
		signer, err := openStoreSigner(cfg)
		if err != nil {
			// A warning rather than a fatal: the operator asked for uploads and
			// cannot have them, which every other route on this install is
			// unaffected by. Failing to boot would take the platform down over
			// a capability nobody is using yet.
			log.Printf("WARNING: store uploads are switched on but the signer could not be "+
				"built, so the /api/v1/store routes are unavailable: %v", err)
		} else {
			storeSigner = signer
			log.Printf("Store uploads: enabled")
		}
	}

	// Durable storage for shared mode: the host directories an operator mounted
	// and declared. Nil when none are, which is the whole of "not offered" —
	// the admin surface does not offer it and a deploy with mappings is refused
	// with the reason. Toft creates directories under a root and never a
	// mount; the mount is the operator's, as the bucket and the filesystem are.
	sharedRoots, err := runtime.ParseStorageRoots(cfg.SharedStorageRoots)
	if err != nil {
		fatalf("SHARED_STORAGE_ROOTS: %v", err)
	}
	// A store is offered on a shared install when the platform can sign for
	// one — there is nothing to mount, so the signer is the whole capability —
	// and in the backing the signer speaks, never one it cannot reach.
	storeBacking := ""
	if storeSigner != nil {
		storeBacking = runtime.BackingMountpoint
		if cfg.StoreBackend == "azblob" {
			storeBacking = runtime.BackingBlob
		}
	}
	sharedStorage := runtime.NewSharedStorage(sharedRoots, cfg.AppsDir, storeBacking)
	if sharedStorage != nil {
		names := make([]string, 0, len(sharedRoots))
		for _, r := range sharedRoots {
			label := r.Name + " (" + r.Path + ")"
			if r.ReadOnly {
				label += " read-only"
			}
			names = append(names, label)
		}
		log.Printf("Shared-mode storage roots: %s", strings.Join(names, ", "))
	}

	// Runtime
	var rt runtime.ServiceRuntime
	// The shared backend over the shared-mode agent. Declared outside the
	// switch because one thing is wired into it *after* the builder exists:
	// the repair of a release whose dependency tree can no longer run on this
	// host. Nil for the mock runtime.
	var agentRT *runtime.AgentRuntime
	// appOutput copies shared apps' output onto the platform's stream; nil
	// on an install that runs no shared workloads.
	var appOutput *appout.Sink

	// Which account an app runs as is also which account it builds as (ADR
	// §29), on every runtime: the build happens on this host in both modes.
	isolation := runtime.Isolation{
		RunAsUser:  cfg.AppRunAsUser,
		PerApp:     cfg.AppUserPerApp,
		UserPrefix: cfg.AppUserPrefix,
		AutoCreate: cfg.AppUserAutoCreate,
		Group:      cfg.AppUserGroup,
		// The number of each auto-created per-app account, recorded once and
		// reused at every start (ADR §122). Set before the copies below are made.
		Numbers: db.AppAccountNumber,
	}
	if !isolation.Enabled() && os.Geteuid() == 0 {
		log.Printf("WARNING: builds run as root, the Toft user: a repository's build code can " +
			"read the platform's own secrets. Set APP_RUN_AS or APP_USER_PER_APP=1 so each app " +
			"builds as the account it runs as.")
	}

	switch cfg.Runtime {
	case "agent":
		if !isolation.Enabled() {
			log.Printf("WARNING: deployed apps run as the Toft user. " +
				"They can read the platform database and every app's secrets. " +
				"Set APP_RUN_AS or APP_USER_PER_APP=1 to separate them.")
		}
		agentRT = runtime.NewAgentRuntime(isolation, resolver, allocator, cfg.AppsDir, cfg.AgentDir).
			WithLogsDir(cfg.LogsDir).
			WithCallbackURL(cfg.CallbackAddr()).
			WithNarrator(platformNarrator{platformLog}).
			WithSharedStorage(sharedStorage)
		// Dialled, and started when nothing answers: from the allowlisted base
		// environment, which it keeps for its life and hands every runner — so
		// it is started here, at boot, and never first from a request path. An
		// agent already running is kept, and with it every app it runs.
		exe, err := os.Executable()
		if err != nil {
			fatalf("Finding the platform binary for the shared-mode agent: %v", err)
		}
		// The bridge's range is picked once and kept (ADR §135): the agent
		// keeps it across its own restarts in its state, and the install keeps
		// it across a reboot — which empties /run — as a setting read back
		// here. AGENT_BRIDGE_CIDR overrides both.
		bridge := cfg.AgentBridgeCIDR
		remembered := ""
		if row, err := db.GetSetting(agentBridgeSetting); err == nil && row != nil {
			remembered = row.Value
		}
		if bridge == "" {
			bridge = remembered
		}
		agentRT.WithMaskDirs(cfg.AppsDir, cfg.LogsDir, cfg.CacheDir, cfg.JobRunDir)
		if err := agentRT.EnsureAgent(exe, runtime.AgentOptions{
			CgroupParent:  cfg.AgentCgroupParent,
			Namespaces:    true,
			PlatformPort:  cfg.Port,
			PlatformHost:  cfg.Host,
			BridgeCIDR:    bridge,
			BlockMetadata: cfg.BlockAWSMetadata,
		}); err != nil {
			fatalf("%v", err)
		}
		agentRT.FollowAgentLog(context.Background())
		if picked := agentRT.Bridge(); picked != "" && picked != remembered {
			if _, err := db.PutSetting(agentBridgeSetting, picked, ""); err != nil {
				log.Printf("remembering the agent's bridge range %s: %v", picked, err)
			}
		}
		if tier, reason := agentRT.Containment(); tier != "" {
			log.Printf("Shared workloads are held at the %s tier%s", tier, map[bool]string{true: "", false: " (" + reason + ")"}[reason == ""])
		}
		appOutput = appout.New(obs.Emit, func() bool {
			st, err := db.GetPlatformSettings()
			return err == nil && st.AppLogCopyUncapped
		})
		appOutput.OnDrop = func(appID, reason string, n int64) {
			telemetry.RecordAppLogDropped(context.Background(), appID, reason, n)
		}
		agentRT.StartOutput(appOutput)
		rt = agentRT
	default:
		rt = runtime.NewMockRuntime(allocator)
	}

	// Ensure directories exist
	os.MkdirAll(cfg.AppsDir, 0755)
	// And that every app account can pass through it, whatever made it first:
	// on a fresh volume it was found 0700 and no app could reach its own tree.
	if err := builder.EnsureTraversable(cfg.AppsDir); err != nil {
		log.Printf("Warning: making %s traversable for app accounts: %v", cfg.AppsDir, err)
	}
	os.MkdirAll(cfg.LogsDir, 0755)
	os.MkdirAll(cfg.CacheDir, 0755)

	jwtSvc := auth.NewJWTService(cfg.JWTSecret)

	// Sign-in providers are administered at runtime (Admin → Authentication),
	// so there is nothing to construct here. What is worth saying at boot is how
	// this install expects anyone to get in — and, when the answer is the
	// built-in administrator's published default password, that leaving it that
	// way means the first person to reach the login page becomes it.
	logSignInBootstrap(cfg, db)

	// The isolated backend, if the operator configured one. Absent, isolated
	// mode is not possible: the job service runs with no backend (its endpoints
	// still answer, with the reason rather than a 404) and a deploy of an app
	// somehow set to isolated fails with an explanation naming the missing
	// configuration.
	isolated := openIsolatedBackend(cfg)

	var (
		jobBackend jobs.Backend
		isolatedRT runtime.ServiceRuntime
		storage    runtime.StorageProvisioner
	)
	// The shared roots are a storage provisioner in their own right, and the
	// admin surface reaches them through exactly the interface it reaches a
	// cluster's bindings through. The isolated backend wins where both exist:
	// an install with a cluster provisions storage in it, and the roots are the
	// answer for the install that has no cluster at all.
	if sharedStorage != nil {
		storage = sharedStorage
	}
	if isolated != nil {
		log.Printf("Isolated backend: %s", cfg.IsolatedTopology())
		// One backend, three roles: it places long-lived workloads, it places
		// one-shot runs, and it binds durable storage to both. They are separate
		// interfaces because the lifecycles differ, not because the cluster does.
		jobBackend = isolated.Jobs()
		isolatedRT = isolated
		storage = isolated
		if kinds := isolated.StorageKinds(); len(kinds) > 0 {
			log.Printf("App storage: %s", strings.Join(kinds, ", "))
		} else {
			log.Printf("App storage is not offered: set KUBE_STORAGE_CLASS for volumes, " +
				"or configure the Mountpoint CSI driver for object stores.")
		}
	}

	// A cluster volume records the class it is bound from (ADR §12a). One that
	// predates that names none and followed KUBE_STORAGE_CLASS at every binding;
	// this writes the class it resolves to today, once, so changing the
	// variable later moves no existing resource.
	if lister, ok := storage.(interface {
		DeclaredClasses() []runtime.DeclaredClass
	}); ok {
		classes := lister.DeclaredClasses()
		n, err := db.RecordStorageClasses(func(backing string) string {
			class, _ := runtime.ResolveDefaultClass(classes, backing)
			return class
		})
		if err != nil {
			log.Printf("WARNING: recording storage classes on existing volumes: %v", err)
		} else if n > 0 {
			log.Printf("Recorded the storage class on %d existing volume(s)", n)
		}
	}

	// One backend per execution mode. This is what makes an app's mode decide
	// where it actually runs, rather than only whether it may enqueue jobs.
	runtimes := runtime.NewRuntimes(rt, isolatedRT)

	// The notification channels, and the failure recorder that leans on them.
	//
	// Hoisted out of the api.NewServer call they used to be constructed inside:
	// every one of the background workers below can notice an app break, and
	// none of them could reach a channel that only the API layer held. The
	// recorder is what they actually take — a worker has no business rendering
	// an email or a Slack block.
	//
	// Both are optional in the strongest sense. With no relay and no bot token
	// configured, the recorder still writes every app_events row; it just tells
	// nobody out of band.
	mailSvc := mail.NewService(db, enc, cfg.BaseURL)
	slackSvc := slack.NewService(db, enc, cfg.BaseURL)
	// The posters, as a list where there was one field. Slack keeps a poster of
	// its own because it has a rendering of its own — blocks and mrkdwn — and
	// the second serves every other kind an administrator can connect, which
	// all take text. Adding a ninth chat tool adds nothing here.
	//
	// The catalog poster is built once and held by the server too, for the
	// platform notifications posted outside the recorder (a publisher request,
	// an account awaiting activation, a widened source policy, an access
	// request) — with only Slack there, a Teams bot carrying the admin
	// audience was a setting that did nothing.
	catalogPoster := api.NewCatalogPoster(db, enc, cfg.BaseURL)
	eventRec := events.NewRecorder(db, mailSvc, slackSvc, catalogPoster)

	// **The one supplier of the entitlement seam** (ADR §32, *Toft is free*):
	// a perpetual enterprise license — every feature entitled, no app cap. It
	// makes no outbound request and holds no goroutine, and no build checks in
	// with anybody.
	//
	// `entitlements.Source` has exactly three consumers — the features admin
	// surface, the dependency policy resolver, and the tier gate that the app
	// cap and the six licensed configuration surfaces read (`api.tier_policy`)
	// — and this is the only thing that supplies them. A fourth consumer added
	// later without being wired falls back to `entitlements.Default()`, which
	// is `Perpetual` too.
	var ents entitlements.Source = entitlements.Perpetual{}

	// The dependency policy, resolved in one place and shared by the builder,
	// the API, the sweep and the CLI (ADR §39).
	depsResolver := &deps.Resolver{Store: db, Ents: ents}

	// **BUILD_ALLOW_SCRIPTS and a blocking dependency policy are mutually
	// exclusive, and the binary refuses to start rather than half-honouring
	// them** — the same treatment half a telemetry pipeline gets.
	//
	// With lifecycle scripts enabled, `npm ci` executes arbitrary code from
	// every package in the tree, *after* the gate has passed and from packages
	// the gate approved only by name. There is no useful meaning to "block
	// rogue packages, and also run their install scripts", and an operator who
	// has both is one who believes they have a control they do not have.
	if cfg.AllowBuildScripts {
		if policy, err := depsResolver.Policy(); err != nil {
			log.Printf("Reading the dependency policy at startup: %v", err)
		} else if policy.Mode == database.DependencyPolicyBlock {
			fatalf("BUILD_ALLOW_SCRIPTS is set and dependency_policy is 'block'. " +
				"These are mutually exclusive: with lifecycle scripts enabled, `npm ci` runs " +
				"arbitrary code from every package in the tree after the gate has approved it " +
				"by name, so the gate is not a control. Unset BUILD_ALLOW_SCRIPTS, or set the " +
				"dependency policy to 'warn' on Admin → Dependencies.")
		}
	}

	// The one reader of a clone credential, built once and shared: the builder
	// clones with it and the auto-deploy poller queries the remote with it, and
	// two instances would be two answers to a question that has one.
	gitCred := gitconn.NewResolver(db, enc)

	// Builder
	// What a delete moved aside and a restart interrupted (builder/trash.go).
	builder.SweepTrash(cfg.AppsDir)
	bld := builder.New(db, runtimes, enc, gitCred, artifacts, builder.Options{
		AppsDir:           cfg.AppsDir,
		JobRunDir:         cfg.JobRunDir,
		LogsDir:           cfg.LogsDir,
		BaseURL:           cfg.AppsBaseURL,
		CacheDir:          cfg.CacheDir,
		AllowBuildScripts: cfg.AllowBuildScripts,
		// The size the worker pool is built to, which is the ceiling the
		// build-concurrency dial cannot exceed.
		MaxConcurrent: cfg.MaxConcurrentBuilds,
		// The operating dials — the ceilings, the scheduler's figures, the R
		// repository and how many builds may start — resolved at each use
		// rather than copied here.
		Dials: dials,

		Events: eventRec,
		Deps:   depsResolver,
		// The derivation behind TOFT_ROLE_KEY. The same service the
		// gateway mints role assertions with, so the key a shared workload is
		// given and the key the proxy signs against are the same derivation
		// rather than two that happen to agree.
		Roles: jwtSvc,
		// A scheduled re-render is announced to the people its owner named.
		// Constructed here rather than inside the builder because it needs the
		// mail service and the authorizer, and a builder that reached for
		// either would make every build test carry both.
		Reports: report.New(db, mailSvc, db),
		// The account an app builds as is the one it runs as (ADR §29).
		BuildAccount: buildAccountFor(isolation),
	})

	// The shared backend's repair, wired here because it needs the builder and
	// the builder needs the backend. A release whose `venv/bin/python` points at
	// an interpreter this host no longer has is rebuilt on the next start rather
	// than exec'd and left crashing until somebody redeploys it — which is the
	// isolated backend's behaviour arriving on the other side of the seam. It
	// costs a healthy tree two os.Stat calls.
	if agentRT != nil {
		agentRT.WithRepairer(bld)
	}
	// A repair's stage marks reach the app's owner; the subprocess stream it
	// runs does not. Wired after construction like the repairer, and only for
	// a repair — a deploy already streams its whole log to a surface the owner
	// reads, and one voice in two places is the duplication this design
	// otherwise argues against.
	bld.WithNarrator(platformLog)

	// The announcement send runner, and its own startup sweep.
	//
	// An announcement still `sending` now belonged to a process that no longer
	// exists. Its pending recipients become `abandoned` and the row completes,
	// so it reads *203 delivered, 0 failed, 47 abandoned* — which is the truth.
	// It never resumes: that needs a durable queue with an at-least-once story,
	// which is the outbox this product has ruled out. And it never sits in
	// `sending`, because a status meaning "we do not know" is how a record
	// starts lying by omission.
	//
	// Synchronous and before the listener, for the same reason the deployment
	// sweep below is: an administrator who opens the page at that moment should
	// be looking at a completed record rather than a live one.
	announcer := announce.New(db, mailSvc)
	if err := announcer.AbandonStranded(); err != nil {
		log.Printf("Abandoning announcements interrupted by the last restart: %v", err)
	}

	// The build queue is in-memory: a deployment still pending or building now
	// was orphaned by the last restart and nothing will ever finish it. Sweep
	// synchronously, before the API can queue new work behind a dead row.
	if n, err := db.FailOrphanedDeployments(); err != nil {
		log.Printf("Failing orphaned deployments: %v", err)
	} else if n > 0 {
		log.Printf("Marked %d deployment(s) interrupted by the last restart as failed", n)
	}

	// Restart apps whose processes died with the last host, or with an agent
	// that lost its state: the DB says running, the agent has nothing. Async so
	// a slow app doesn't delay the listeners; until an app is restored its
	// requests 502. A real shared backend only: the mock runtime has no real
	// processes, so "restoring" against it would
	// just mark every running app crashed when its port never answers.
	// Before anything is served: a hostname a changed BASE_URL now collides
	// with is switched off and its app moved back to /apps/{slug}.
	bld.RetireInvalidHostnames(cfg.ReservedHosts(), cfg.ExtraOriginHosts())

	if config.SharedHostRuntime(cfg.Runtime) {
		go bld.Reconcile()
	}

	// Measure releases deployed before sizes were recorded. Async and
	// best-effort for the same reason as the reconcile above: it walks
	// directories and may touch the artifact store, and nothing depends on the
	// answer being ready before the listeners are.
	go bld.BackfillReleaseSizes()

	// Resource accounting. The shared collector samples app processes out of
	// band — never from a request path — and the roll-up sweep folds the fine
	// intervals into daily history.
	consumption := resource.NewRecorder(db)
	collector := resource.NewSharedCollector(consumption, func() ([]resource.Workload, error) {
		return listWorkloads(db, rt)
	})
	collector.Start()
	go rollUpConsumption(db)
	// App failure events age out on the same hourly goroutine rather than a
	// ticker of their own: it is one DELETE a day against an indexed column.
	go pruneAppEvents(db)
	// The platform's account of each app, on its own clock. Beside the events
	// rather than folded into them: ninety days of every start's stage marks
	// is a different quantity from ninety days of failures, and shortening one
	// is not asking to shorten the other.
	go prunePlatformLog(db)
	// Who did what and who opened which app, on one dial that keeps
	// everything until an administrator chooses a number (ADR §46).
	go pruneAccessRecord(db)
	// The send ledger ages out on its own schedule, because its retention is an
	// administrator's setting rather than a constant: it is the only record of
	// what apps have emailed, and how long an install must be able to answer
	// for that is a compliance decision, not ours.
	go pruneIntegrationSends(db)
	// Who each announcement was mailed to, on its own clock and its own
	// setting. It prunes the per-person rows and never the announcement or its
	// counts: those are the install's own record of what it told people, and
	// there is no date on which that stops being worth having.
	go pruneAnnouncementRecipients(db)

	// The host backend (ADR §106): where a run goes when isolated execution is
	// not available, on any install that runs shared-mode workloads at all. It
	// shares the shared runtime's resolver, repairer, account policy and storage
	// roots, so a host run and the app's own process see the same release as the
	// same account.
	var hostJobs jobs.Backend
	if agentRT != nil {
		// Placed, watched and held by the agent (ADR §133): it
		// owns the cgroups and the watchdog, so there is nothing to turn on here.
		hj, err := agentRT.HostJobs(cfg.CallbackAddr(), jwtSvc.MintHostBootstrapToken, cfg.JobRunDir)
		if err != nil {
			log.Printf("Host job runs unavailable: %v", err)
		} else {
			if hj.HostMemoryEnforcement() != "cgroup" {
				log.Printf("Host job runs: memory limits are sampled by the agent, not enforced by the kernel; " +
					"set AGENT_CGROUP_PARENT to a delegated cgroup, or run Toft where it may create cgroups")
			}
			hostJobs = hj
		}
	}
	// A node id that survives a restart, so boot Reconcile can tell the
	// previous process's unstarted claims from another replica's and return
	// them to the queue at once. Each replica of a chart install is its own
	// pod and so its own hostname.
	nodeID, _ := os.Hostname()
	jobSvc := jobs.New(db, jobBackend, jobs.Options{
		NodeID: nodeID,
		Events: eventRec,
		Dials:  dials,
		Host:   hostJobs,
		// A nil store converts to a nil interface, so an install with none
		// passes nil and the sweep has nothing to remove.
		Outputs: artifacts,
	})
	jobSvc.Start()
	telemetry.ObserveJobQueue(func() int64 {
		n, err := db.CountQueuedJobRuns()
		if err != nil {
			log.Printf("telemetry: counting queued job runs: %v", err)
			return 0
		}
		return n
	})
	defer jobSvc.Close()

	// A document's scheduled re-render. The second loop over the shared claim,
	// and the only one that fires a *deploy* — which is why it lives beside the
	// build queue it feeds. The third, the scheduled restart, is started below
	// once the server it calls into exists.
	//
	// Started whatever the estate holds, like the dormancy sweep: the claim is
	// one indexed query against a table that is empty on an install with no
	// notebooks, and a loop that has to be restarted after the first notebook
	// is deployed is a loop somebody will forget to restart.
	renderScheduler := builder.NewRenderScheduler(db, bld, eventRec)
	renderScheduler.Start()
	defer renderScheduler.Close()

	// Idle sleep. The tracker is shared with the API server — the proxy is the
	// only thing that knows an app was used, and the reaper is the only thing
	// that acts on it, so a second tracker would see an estate nobody had ever
	// visited. Both are inert until an administrator sets a timeout.
	idleTracker := idle.New()
	// One WakeGroup for the gateway and the reaper's end-of-window queue, so a
	// visitor and the queue reaching the same isolated app share one start.
	wakeGroup := idle.NewWakeGroup()
	idleReaper := idle.NewReaper(db, runtimes, idleTracker).WithNarrator(platformLog).
		WithWaker(wakeGroup, bld.Wake)
	idleReaper.Start()
	defer idleReaper.Close()

	// Dormant account locking. Started whatever the setting says, for the reason
	// the gallery syncer is: the sweep reads the switch on every pass, so an
	// administrator turning it on does not have to wait for a restart, and
	// while it is off the pass costs one settings read and touches nobody.
	dormancySweeper := dormancy.New(db)
	dormancySweeper.Start()
	defer dormancySweeper.Close()

	// This package is two things, and only one of them is isolated-only.
	//
	// **Crash recovery** is: with no cluster there is no workload it could
	// restart, and a loop that can only ever decline is worse than no loop.
	// Shared mode's restarts are the agent's, in place, as the kubelet's are a
	// pod's — so `Start`, which runs that loop, is still conditional.
	//
	// **`Remount`** is not. It is "start these apps and record what failed",
	// and since a credential detached from an app has to reach a *shared*
	// workload too (ADR §12b), a nil supervisor on a shared-only install made
	// that revocation a silent no-op — the API answered `restarting: [...]` and
	// nothing restarted. Constructed always, started only where the loop has
	// something to do; both recovery paths decline a shared app on their own,
	// so an unconditional constructor changes nothing about what recovers.
	workloadSupervisor := supervisor.New(db, bld, eventRec).WithNarrator(platformLog)
	if runtimes.IsolatedConfigured() {
		workloadSupervisor.Start()
		defer workloadSupervisor.Close()
	}

	// Crash detection. Restricted to a real shared backend for the same reason
	// Reconcile is: the mock runtime has no processes, so watching it would
	// report on a simulation. Isolated workloads are watched either way — that
	// backend's List is the cluster's own view.
	//
	// The watcher decides and the supervisor acts: one callback, invoked after
	// the status write, and nil on an install that has no supervisor.
	healthWatcher := health.New(db, runtimes, eventRec, config.SharedHostRuntime(cfg.Runtime), workloadSupervisor.Notice).
		WithNarrator(platformLog).
		WithCPULimit(func(app *database.App) int {
			return dials.Envelope().ResourcesForApp(app.MemoryLimitMB, app.CPULimitMillicores).CPUMillicores.Value
		})
	if config.SharedHostRuntime(cfg.Runtime) {
		// The boot reconcile above restores apps for minutes; until it has
		// been through them all, an app not yet reached is not a crash.
		healthWatcher.WithHold(bld.Reconciled())
	}
	healthWatcher.Start()
	defer healthWatcher.Close()

	// Auto-deploy on git push (polling; see internal/autodeploy)
	//
	// Kept in a variable so the API server can hand it a verified webhook
	// delivery below. Nil with polling off, and the route then verifies,
	// counts, and does nothing — which is the same outcome as no delivery at
	// all, because the webhook is an accelerator and never the mechanism.
	var poller *autodeploy.Poller
	if cfg.AutoDeployPollSeconds > 0 {
		poller = autodeploy.New(db, bld, gitCred, eventRec,
			time.Duration(cfg.AutoDeployPollSeconds)*time.Second, dials)
		poller.Start()
	} else {
		log.Printf("Auto-deploy polling disabled (AUTO_DEPLOY_POLL_INTERVAL=0)")
	}

	// The gallery catalog. Started whatever the setting says, because starting
	// it costs one cached-document read and no network: while the mode is off
	// it makes no outbound request at all, which is what keeps a fresh install
	// from phoning home to a public repository nobody asked it to contact.
	gallerySource := "built-in"
	if os.Getenv("GALLERY_CATALOG_URL") != "" {
		gallerySource = "environment"
	}
	gallerySvc := gallery.New(db, cfg.GalleryCatalogURL, gallerySource, version.Version)
	gallerySvc.Start()
	defer gallerySvc.Stop()

	// The advisory feed and the sweep over serving releases (ADR §39). Both are
	// started whatever the setting says, for the gallery's reason: starting
	// them costs a goroutine parked on a timer, and each asks the policy fresh
	// on every tick — so **off means no outbound request and no work at all**,
	// including for a tick already scheduled, and an administrator switching
	// the feature on does not have to restart the binary to get it.
	// Snapshots are spooled on APPS_DIR's disk, for the artifact store's reason.
	advisoryScratch := artifactStaging(cfg)
	advisorySvc := advisory.New(db, func() (advisory.Config, error) {
		policy, err := depsResolver.Policy()
		if err != nil {
			return advisory.Config{}, err
		}
		settings, err := db.GetPlatformSettings()
		if err != nil {
			return advisory.Config{}, err
		}
		// The mirror credential, decrypted here because only this layer holds
		// the encryptor. It never reaches a workload: runtime.BaseEnv() is an
		// allow-list, and this value is not on it.
		credential, err := decryptFeedCredential(enc, db)
		if err != nil {
			log.Printf("advisory: the stored feed credential could not be read (%v) — "+
				"re-enter it on Admin → Dependencies", err)
		}
		return advisory.Config{
			Enabled:    !policy.Off(),
			Endpoint:   settings.DependencyFeedEndpoint,
			Credential: credential,
			Ecosystems: advisory.Synced,
			Interval:   time.Duration(settings.DependencyFeedIntervalHrs) * time.Hour,
			ScratchDir: advisoryScratch,
		}, nil
	})
	advisorySvc.Start()
	defer advisorySvc.Stop()

	// The corpus's age per ecosystem, read on the export interval and never on
	// a request path. An ecosystem that has never loaded reports nothing at all
	// rather than zero: zero would read as perfectly fresh, which is the exact
	// opposite of the truth.
	telemetry.ObserveDependencyFeedAge(func() map[string]float64 {
		states, err := db.FeedStates()
		if err != nil {
			log.Printf("telemetry: reading advisory feed ages: %v", err)
			return nil
		}
		out := map[string]float64{}
		for _, st := range states {
			if age, ok := st.Age(); ok {
				out[st.Ecosystem] = age.Seconds()
			}
		}
		return out
	})

	// The backend is wired in because an automatic lock is an automatic stop:
	// a locked app still running the package the lock was placed for is a label
	// rather than a control.
	//
	// The release trees are how it reads a shared R app's library for the
	// packages the app installed while it ran — the one thing it reads off disk.
	depSweeper := depsweep.NewSweeper(db, depsResolver, eventRec).WithRuntimes(runtimes).
		WithReleaseTrees(func(ref string) string {
			return builder.ServingTreeDir(cfg.AppsDir, runtime.ParseReleaseRef(ref))
		})
	depSweeper.Start()
	defer depSweeper.Stop()

	// API server
	// The host data plane: people reaching a directory under a host root with
	// `toft store` and the Files page, through URLs the platform signs for
	// itself and serves. Offered only where a folder's gate can keep an app
	// out — every app on an account of its own — and where the kernel stops an
	// app hard-linking a file it cannot read into its folder for a person to
	// download through root. Absent otherwise, with the reason on Admin →
	// Checks.
	hostStore, hostStoreAbsent := openHostStore(cfg, sharedRoots, db)

	srv := api.NewServer(cfg, db, jwtSvc, runtimes, bld, enc,
		// The level obs installed, so a settings write reaches this process's
		// own log without restarting it.
		api.WithLogLevel(logLevel),
		// Absent, every gallery route 404s and nothing about it appears in the
		// UI — the same answer as an administrator switching it off.
		api.WithGallery(gallerySvc),
		// The same resolver the builder and the poller hold. One instance, so
		// that disabling a connection drops the one cache rather than one of
		// three.
		api.WithGitConnections(gitCred),
		// POST /apps asks the remote before it claims a slug, so a missing
		// repository or branch is a refusal rather than a failed app.
		api.WithAppAccounts(isolation),
		api.WithRepoPreflight(func(ctx context.Context, repoURL, branch, token string) error {
			_, err := builder.LsRemote(ctx, repoURL, branch, token)
			return err
		}),
		api.WithSubdirPreflight(builder.SubdirExists),
		api.WithArtifacts(artifacts),
		// Absent, the storage routes 404 rather than existing and failing:
		// absent configuration means the capability is not offered at all.
		api.WithStorage(storage),
		// The other half of a store: a person with a grant and no mount. Absent
		// unless STORE_UPLOADS is set, because that is also the moment the
		// platform gains its first data-plane permission on a store's bucket.
		api.WithStoreSigner(storeSigner),
		api.WithHostStore(hostStore, hostStoreAbsent),
		// The same recorder the shared collector feeds: a self-reporting
		// isolated workload and a sampled shared one must land in one metric
		// model, or a mixed install cannot be compared.
		api.WithConsumption(consumption),
		api.WithJobs(jobSvc),
		// The same sink the writers above were built with. The tail reads what
		// it stored and the live socket subscribes to what it is about to
		// store; two sinks would make a pane that showed neither.
		api.WithPlatformLog(platformLog),
		// The proxy's half of idle sleep: without it nothing records that an
		// app was used, and the reaper above would have nothing to read.
		api.WithIdle(idleTracker),
		api.WithWakeGroup(wakeGroup),
		// The recovery loop, so the crashed interstitial can stop promising a
		// refresh once it has stopped trying, and so a person pressing Restart
		// clears what it counted.
		api.WithSupervisor(workloadSupervisor),
		api.WithHealth(healthWatcher),
		// Email is optional in the strongest sense: with no relay configured
		// the service reports itself disabled, every notification is dropped
		// with a log line, and nothing that would have been announced is lost —
		// it is all still in the UI.
		api.WithMailer(mailSvc),
		// The one message this platform sends whose arrival is the point. It
		// takes the same mailer, and needs it: with no relay the runner reports
		// itself unavailable and Send is absent rather than disabled.
		api.WithAnnouncer(announcer),
		// Slack is optional the same way, and absent rather than dormant: with
		// no bot token the routes explain themselves, nothing is posted, and no
		// app can be given a channel.
		api.WithSlack(slackSvc),
		// Every other kind's poster, for the platform notifications the server
		// posts itself. Never Slack: postPlatform asks it separately.
		api.WithPosters(catalogPoster),
		// Which third-party packages this install will run: the policy
		// resolver the builder also holds, the corpus syncer behind Sync now,
		// and the sweeper behind Check again. One option, because they are one
		// capability — a page whose refresh button did nothing would be worse
		// than no page.
		api.WithDependencies(depsResolver, advisorySvc, depSweeper),
		// Gate 1 of the four, and the tier every other gate reads. The other
		// consumer is depsResolver above; the third is inside the server, where
		// `tier_policy.go` reads this same source for the app cap and the six
		// licensed configuration surfaces.
		api.WithEntitlements(ents),
		// The license surface: the check-in behind *Check now*, and the three
		// facts the payload preview renders. Not a third consumer of the
		// entitlement seam — this one renders the license rather than gating on
		// it.
		// What each app's page says about where its output goes. Only when
		// there is a copy: a nil sink inside the interface would be a value.
		appOutputOption(appOutput),
	)
	// A verified webhook delivery from a connected git host marks apps for an
	// immediate pass. Wired after construction because the poller holds the
	// builder and the route hangs off the server, so one of the three has to be
	// — and this is the one whose absence changes nothing.
	if poller != nil {
		srv.SetAutodeploy(poller)
	}
	// An app's scheduled restart, `[restart]` in its toft.toml. Beside the
	// server because what it fires is the server's restart — the wake group,
	// the status write and the audit row — without the forgiveness a person's
	// restart carries.
	restartScheduler := api.NewRestartScheduler(srv, eventRec)
	restartScheduler.Start()
	defer restartScheduler.Close()

	go srv.WatchPool()              // the Database pool check's recent-waits window
	go srv.FinishStorageDeletions() // deletes whose volumes the cluster has not finished
	router := srv.Router(spa.Handler())

	// Start HTTP server
	addr := cfg.Addr()
	log.Printf("Toft %s starting on %s", version.String(), addr)
	log.Printf("Base URL: %s", cfg.BaseURL)
	log.Printf("Runtime: %s", cfg.Runtime)

	// Session cookies carry the Secure flag by default. Over plain http on a
	// real host the browser will never send them back — say so instead of
	// letting logins fail silently.
	if u, err := url.Parse(cfg.BaseURL); err == nil && u.Scheme != "https" && !config.IsLoopbackHost(u.Hostname()) && !cfg.DevInsecureCookies {
		log.Printf("WARNING: BASE_URL %s is plain http on a non-loopback host. "+
			"Session cookies are marked Secure and browsers will not send them over http — "+
			"serve https, or set DEV_INSECURE_COOKIES=1 (dev only).", cfg.BaseURL)
	}
	if cfg.DevInsecureCookies {
		log.Printf("WARNING: DEV_INSECURE_COOKIES is set — session cookies ship without the " +
			"Secure flag and are recoverable from any cleartext request. Dev only.")
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Deployed apps are served from their own listener so they land on a
	// different browser origin than the platform.
	var appsServer *http.Server
	if cfg.SeparateAppsListener() {
		appsServer = &http.Server{
			Addr:              cfg.AppsAddr(),
			Handler:           srv.AppsRouter(),
			ReadHeaderTimeout: 15 * time.Second,
		}
		log.Printf("Apps gateway on %s (%s)", appsServer.Addr, cfg.AppsBaseURL)
		// One host, two ports: the origins differ and the cookie jar does not.
		// Said at every start because nothing in the code can close it — the
		// gateway relies on that very jar for its session (ADR §2).
		if !cfg.TwoHostMode() {
			log.Printf("NOTE: apps are served on another port of the platform's own host (%s). "+
				"Browsers keep one cookie jar per host, so a deployed app's page can plant a "+
				"session cookie that signs a signed-out viewer in as someone else. Serve apps "+
				"from their own hostname (APPS_BASE_URL) to close it.", cfg.AppsBaseURL)
			if api.HSTSWithheld(cfg) {
				log.Printf("NOTE: no Strict-Transport-Security is sent on either origin. The platform "+
					"(%s) and the apps (%s) share one host under different schemes, and HSTS ignores "+
					"the port: a browser told https by one would refuse the other for a year. Serve "+
					"both over https, or apps from their own hostname, to send it.", cfg.BaseURL, cfg.AppsBaseURL)
			}
		}
	} else {
		log.Printf("WARNING: apps share the platform origin (APPS_PORT=%d). "+
			"A deployed app can call the platform API as its viewer.", cfg.AppsPort)
	}

	// The workload callback listener (CALLBACK_ADDR), absent unless set. The
	// same router behind a filter that admits only the runner and app subtrees
	// and drops every forwarded header, so an isolated install can confine the
	// apps namespace to this port and keep it off the browser-facing :8080
	// (ADR §31 D4).
	var callbackServer *http.Server
	if cfg.CallbackListenAddr != "" {
		callbackServer = &http.Server{
			Addr:              cfg.CallbackListenAddr,
			Handler:           api.CallbackHandler(router),
			ReadHeaderTimeout: 15 * time.Second,
		}
		log.Printf("Workload callbacks on %s (/api/v1/runner, /api/v1/app only)", callbackServer.Addr)
	}

	// Graceful drain.
	//
	// Toft is permanently in the data path for every app request and runs
	// as a single instance, so closing the listeners outright drops live app
	// connections on every upgrade. Draining is what reduces routine
	// maintenance from an outage to a pause; with one instance in the path it
	// is a correctness requirement, not a nicety.
	//
	// Scope is proxy traffic only. In-flight builds and cron dispatches are
	// re-adopted by the reconciler on the way back up, which is a mechanism
	// that already exists and already has to work after an unplanned restart.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("Shutting down: draining in-flight requests (up to %s)...", drainTimeout)

		// The apps gateway drains first and is listed first for a reason: it
		// is the listener carrying user-visible app traffic, and it should get
		// the whole budget rather than share it with the platform API.
		drainServers(drainTimeout, namedServer{"apps gateway", appsServer}, namedServer{"platform API", httpServer},
			namedServer{"workload callbacks", callbackServer})

		srv.FlushUsage() // don't lose the last interval's usage counters
		// The app output tailer reads every file to its end once more, so a
		// line printed during the drain is copied rather than left for the
		// next start's position to resume from.
		if agentRT != nil {
			agentRT.StopOutput()
		}
		collector.Stop()
		consumption.Close() // flushes; at most one interval is ever at risk

		// Telemetry drains last and under a cap of its own. A binary that will
		// not stop because a collector is down is worse than a lost batch, and
		// five seconds is comfortably inside whatever the supervisor allows
		// after the thirty the listeners just spent.
		telemetryCtx, cancelTelemetry := context.WithTimeout(context.Background(), telemetryDrainTimeout)
		if err := stopTelemetry(telemetryCtx); err != nil {
			log.Printf("Telemetry did not drain within %s: %v — abandoning the last batch",
				telemetryDrainTimeout, err)
		}
		cancelTelemetry()

		log.Printf("Drained.")
	}()

	if appsServer != nil {
		go func() {
			if err := appsServer.ListenAndServe(); err != http.ErrServerClosed {
				fatalf("Apps gateway error: %v", err)
			}
		}()
	}

	if callbackServer != nil {
		go func() {
			if err := callbackServer.ListenAndServe(); err != http.ErrServerClosed {
				fatalf("Workload callback listener error: %v", err)
			}
		}()
	}

	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		fatalf("HTTP server error: %v", err)
	}
}

// telemetryOptions assembles what the pipeline needs beyond the environment.
//
// The two interesting arguments are the environment tag and the secret list.
//
// `deployment.environment.name` comes from the environment_tag *setting*, which
// is runtime-changeable everywhere else in this product and is read exactly once
// here. That inconsistency is deliberate and small: a resource attribute is
// resolved at startup by construction, an operator who renames their
// environment is doing it during a change window anyway, and the alternative —
// a DEPLOYMENT_ENVIRONMENT variable — asks every install to say the same thing
// twice. It is stated in ADR §31 and in `docs/dev-reference.md` § Telemetry so
// nobody has to discover it.
//
// The secrets are what the exception scrubber must never let out. They are
// passed from live config rather than listed inside internal/telemetry, so a
// secret added to config.Config and passed here is scrubbed for free — and one
// that is added and *not* passed here shows up in TestExceptionMessageScrubbed
// as a visible gap rather than as a silent leak.
func telemetryOptions(cfg *config.Config, db *database.DB) telemetry.Options {
	opts := telemetry.Options{
		Env:           cfg.Telemetry,
		Version:       version.Version,
		Revision:      version.Revision(),
		ExecutionMode: executionModeLabel(cfg),
		Secrets: []string{
			cfg.JWTSecret,
			cfg.EncryptionKey,
			cfg.Telemetry.Headers,
			cfg.Telemetry.MetricsToken,
			cfg.DatabaseURL,
			cfg.GitHubClientSecret,
			cfg.OIDCClientSecret,
			cfg.DevPassword,
		},
	}
	// A retired key still opens everything written before the rotation, so it
	// is scrubbed on exactly the same terms as the live one. Appended rather
	// than listed above because it is a list of its own.
	opts.Secrets = append(opts.Secrets, cfg.EncryptionKeyOld...)
	// Best effort: a settings read that fails is not a reason to have no
	// telemetry, and an unlabelled environment is the same state every install
	// that has not set one is already in.
	if settings, err := db.GetPlatformSettings(); err == nil {
		opts.Environment = settings.EnvironmentTag
	}
	return opts
}

// executionModeLabel names what this install can place, for the resource.
//
// `mixed` is the honest answer for an install with an isolated backend
// configured: it can still run shared apps, and most do. A resource attribute
// that claimed `isolated` would make an operator's "where does this run"
// dashboard wrong for every app that had not been promoted.
//
// It asks about Kubernetes rather than about IsolatedConfigured() because
// `ECS_CLUSTER` names a backend that does not exist (openIsolatedBackend warns
// and returns nil; ADR §26). Labelling such an install `mixed` would describe
// placement that never happens, and this attribute is resolved once at startup
// — before the backend is opened — so it cannot ask the runtimes the way
// api.capabilities() does.
func executionModeLabel(cfg *config.Config) string {
	if cfg.KubernetesConfigured() {
		return "mixed"
	}
	return "shared"
}

// decryptTelemetryHeaders turns the stored ciphertext into the credential the
// exporter needs. main holds the encryptor at boot; afterwards the API layer
// does, which is why this exists in both places and in neither one twice.
// decryptFeedCredential reads the advisory mirror's credential, which is its
// own encrypted document for the reason the SMTP password and the collector
// headers are: the settings blob is audited whole, and a credential must never
// appear in an audit row.
//
// It is read here and handed to the syncer, and it goes nowhere else. In
// particular it never reaches a workload — runtime.BaseEnv() is an allow-list,
// and this is not on it.
// reportUnreadableSecrets says once, at boot, what a wrong or lost
// ENCRYPTION_KEY has cost — instead of letting the operator learn it one
// failure at a time, when a build fails at 2am and then again when mail stops
// and then again when Slack stops.
//
// It is **never fatal**, and that is a decision rather than a leniency (ADR
// §62). The platform coming up is the only state from which an administrator
// can re-enter anything, nothing in the authentication path uses this key — a
// local password is PBKDF2, an API key is stored as a SHA-256, a session is
// signed with JWT_SECRET — so a lost key locks nobody out, and refusing to boot
// would turn a recoverable settings problem into an outage.
//
// Silent when everything reads. A line every boot saying nothing is wrong is
// how operators learn to skim past the boot output, which is where the fatal
// errors are.
func reportUnreadableSecrets(enc *crypto.Encryptor, db *database.DB) {
	byClass := map[string]int{}
	total, unreadable := 0, 0
	err := db.EachEncryptedSecret(func(s database.EncryptedSecret) error {
		total++
		if !enc.CanRead(s.Record) {
			unreadable++
			byClass[s.Class]++
		}
		return nil
	})
	if err != nil {
		// Best effort. An install whose database cannot be walked has a larger
		// problem than this report, and it is about to say so elsewhere.
		log.Printf("encryption: could not check what the keys can read: %v", err)
		return
	}
	if unreadable == 0 {
		return
	}

	classes := make([]string, 0, len(byClass))
	for class, n := range byClass {
		classes = append(classes, fmt.Sprintf("%s ×%d", class, n))
	}
	sort.Strings(classes)
	log.Printf("encryption: %d of %d stored secrets cannot be read with this install's keys (%s) — "+
		"they were written under a key that is in neither ENCRYPTION_KEY nor ENCRYPTION_KEY_OLD. "+
		"Put that key back, or re-enter those credentials; `toft rotate-key --dry-run` lists them",
		unreadable, total, strings.Join(classes, ", "))
}

func decryptFeedCredential(enc *crypto.Encryptor, db *database.DB) (string, error) {
	ciphertext, err := db.GetDependencyFeedCredential()
	if err != nil || ciphertext == "" || enc == nil {
		return "", err
	}
	return enc.Decrypt(ciphertext)
}

func decryptTelemetryHeaders(enc *crypto.Encryptor, doc database.TelemetrySettings) (string, error) {
	if doc.Headers == "" || enc == nil {
		return "", nil
	}
	return enc.Decrypt(doc.Headers)
}

// logTelemetry reports what is exporting, in one line, at the level of detail
// the startup log gives everything else.
//
// The endpoint is named; the credential never is. That is the same rule the
// admin page follows and the reason OTEL_EXPORTER_OTLP_HEADERS is treated as a
// credential everywhere in this codebase: it may carry a bearer token for the
// operator's observability stack.
//
// It names the *source* too, because on an install where both exist the
// difference is the first thing somebody needs when the endpoint in the log is
// not the one in their unit file.
func logTelemetry(cfg *config.Config, rt telemetry.Runtime) {
	if !rt.Enabled() && !cfg.Telemetry.ScrapeEnabled() {
		return
	}
	var parts []string
	if rt.Traces {
		parts = append(parts, fmt.Sprintf("traces %.0f%% sampled", rt.SampleRatio*100))
	}
	if rt.Metrics {
		parts = append(parts, fmt.Sprintf("metrics every %s", rt.MetricInterval))
	}
	if len(parts) > 0 {
		log.Printf("Telemetry: %s → %s (from %s)",
			strings.Join(parts, ", "), telemetry.EndpointHost(rt.Endpoint), rt.Source)
	} else if rt.Source == telemetry.SourceSettings {
		// A saved document with export switched off. Said out loud, because an
		// operator who can see OTEL_EXPORTER_OTLP_ENDPOINT in the environment
		// would otherwise have no way to know why nothing is arriving.
		log.Printf("Telemetry: export is switched off in Admin → Telemetry (the OTEL_* environment is not used)")
	}
	if warning := cfg.Telemetry.MetricsBindWarning(); warning != "" {
		log.Printf("WARNING: %s", warning)
	}
}

// listWorkloads maps the process manager's live processes back to the apps
// they belong to, which is the one piece of the collector only the platform
// can supply: the runtime knows PIDs, the database knows ownership.
// agentBridgeSetting is where the install remembers the agent's bridge range,
// so a reboot — which empties the agent's state — does not move it.
const agentBridgeSetting = "agent_bridge_cidr"

func listWorkloads(db *database.DB, rt runtime.ServiceRuntime) ([]resource.Workload, error) {
	metrics, err := rt.List()
	if err != nil {
		return nil, err
	}
	apps, err := db.ListAllApps()
	if err != nil {
		return nil, err
	}

	out := make([]resource.Workload, 0, len(apps))
	for i := range apps {
		app := &apps[i]
		m, ok := metrics[app.ProcessName()]
		// A runner reporting from a cgroup of its own is billed by its own
		// report; sampled here as well it would be billed twice.
		if !ok || m.PID <= 0 || m.SelfReported {
			continue
		}
		out = append(out, resource.Workload{
			AppID:   app.ID,
			Name:    app.ProcessName(),
			PID:     m.PID,
			Wrapped: m.Wrapped,
		})
	}
	return out, nil
}

// rollUpConsumptionInterval is how often fine intervals are folded into daily
// history. Hourly is frequent enough that the interval table stays small and
// infrequent enough that the sweep is never contending with a flush.
const rollUpConsumptionInterval = time.Hour

// How often the failure-event sweep runs. How *long* an event is kept is a
// platform setting, read per pass — see pruneAppEvents.
const appEventPruneInterval = 24 * time.Hour

// rollUpConsumption folds intervals older than their retention into the daily
// table. Safe to run on several nodes: the fold and the delete are one
// transaction, so a sweep that races another simply finds nothing to do.
func rollUpConsumption(db *database.DB) {
	t := time.NewTicker(rollUpConsumptionInterval)
	defer t.Stop()
	for {
		rollUpConsumptionPass(db)
		<-t.C
	}
}

// rollUpConsumptionPass is one pass, split out so the sweep's duration is a
// deferred measurement rather than a figure computed on every branch of the
// body. Every timed sweep in this file has the same shape for the same reason.
func rollUpConsumptionPass(db *database.DB) {
	cutoff := time.Now().UTC().Add(-database.IntervalRetention)
	ctx, span := telemetry.Start(context.Background(), "consumption.sweep",
		telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepConsumption, outcome, time.Since(start))
	}()

	res, err := db.RollUpConsumption(cutoff)
	if err != nil {
		log.Printf("consumption roll-up: %v", err)
		return
	}
	outcome = telemetry.OutcomeSucceeded
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepConsumption,
		telemetry.Removed{Rows: res.Removed})
	if res.Folded > 0 {
		log.Printf("consumption roll-up: folded %d app-day(s) into history", res.Folded)
	}
}

// pruneAppEvents ages out failure events, on the administrator's
// app_event_retention_days (90 by default, never below a week — the table is
// what answers "did this app fail over the weekend").
//
// Read per pass rather than at startup, like the send ledger's beside it, so an
// administrator who shortens retention does not have to wait for a restart to
// see it applied. A settings read that fails skips the pass rather than falling
// back to the default: a sweep is a delete, and guessing a cutoff is the one
// way this can destroy history nobody asked it to.
func pruneAppEvents(db *database.DB) {
	t := time.NewTicker(appEventPruneInterval)
	defer t.Stop()
	for {
		pruneAppEventsPass(db)
		<-t.C
	}
}

func pruneAppEventsPass(db *database.DB) {
	ctx, span := telemetry.Start(context.Background(), "prune.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepPrune, outcome, time.Since(start))
	}()

	settings, err := db.GetPlatformSettings()
	if err != nil {
		log.Printf("app event prune: reading settings: %v", err)
		return
	}
	cutoff := time.Now().UTC().Add(-settings.AppEventRetention())
	span.Set(telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))

	n, err := db.DeleteAppEventsBefore(cutoff)
	if err != nil {
		log.Printf("app event prune: %v", err)
		return
	}
	outcome = telemetry.OutcomeSucceeded
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepPrune, telemetry.Removed{Rows: n})
	if n > 0 {
		log.Printf("app event prune: removed %d event(s) older than %d days",
			n, settings.AppEventRetentionDays)
	}
}

// pruneAccessRecord cuts audit_log and the two usage rollups on
// access_record_retention_days. The pass is database.PruneAccessRecord; this
// only schedules it — daily, and an hour after a pass that ran out of its
// budget, so cutting years on a busy install takes hours rather than weeks.
//
// The events prune's three disciplines: settings read per pass, a failed read
// skips the pass rather than guessing a cutoff, and 0 touches nothing.
func pruneAccessRecord(db *database.DB) {
	for {
		next := appEventPruneInterval
		if pruneAccessRecordPass(db) {
			next = time.Hour
		}
		time.Sleep(next)
	}
}

// accessRecordBudget is how long one pass may spend deleting.
const accessRecordBudget = 5 * time.Minute

func pruneAccessRecordPass(db *database.DB) (truncated bool) {
	ctx, span := telemetry.Start(context.Background(), "access_record.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepAccessRecord, outcome, time.Since(start))
	}()

	settings, err := db.GetPlatformSettings()
	if err != nil {
		log.Printf("access record prune: reading settings: %v", err)
		return false
	}
	pass, err := db.PruneAccessRecord(settings.AccessRecordRetentionDays, accessRecordBudget)
	if err != nil {
		log.Printf("access record prune: %v", err)
		return false
	}
	outcome = telemetry.OutcomeSucceeded
	if settings.AccessRecordRetentionDays == 0 {
		return false
	}
	span.Set(telemetry.AttrSweepCutoff.String(pass.Cutoff.Format(time.RFC3339)))
	if pass.Truncated {
		span.Set(telemetry.AttrSweepTruncated.Bool(true))
	}
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepAccessRecord, telemetry.Removed{Rows: pass.Total()})
	if n := pass.Total(); n > 0 {
		more := ""
		if pass.Truncated {
			more = " — more remain, next pass in an hour"
		}
		log.Printf("access record prune: removed %d row(s) older than %d days%s",
			n, settings.AccessRecordRetentionDays, more)
	}
	return pass.Truncated
}

// prunePlatformLog ages out the platform's own account of each app, on the
// administrator's platform_log_retention_days.
//
// The three disciplines beside it are the events prune's and are held for the
// same reasons. Read per pass rather than at startup, so an administrator who
// shortens retention does not have to wait for a restart. A settings read that
// fails skips the pass rather than falling back to the default — a sweep is a
// delete, and guessing a cutoff is the one way this destroys history nobody
// asked it to. And its own sweep name, because two sweeps with two clocks and
// two cutoffs would otherwise be indistinguishable in the operator's metric,
// which is the one question that metric is for.
func prunePlatformLog(db *database.DB) {
	t := time.NewTicker(appEventPruneInterval)
	defer t.Stop()
	for {
		prunePlatformLogPass(db)
		<-t.C
	}
}

func prunePlatformLogPass(db *database.DB) {
	ctx, span := telemetry.Start(context.Background(), "platformlog.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepPlatformLog, outcome, time.Since(start))
	}()

	settings, err := db.GetPlatformSettings()
	if err != nil {
		log.Printf("platform log prune: reading settings: %v", err)
		return
	}
	cutoff := time.Now().UTC().Add(-settings.PlatformLogRetention())
	span.Set(telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))

	n, err := db.DeletePlatformLogBefore(cutoff)
	if err != nil {
		log.Printf("platform log prune: %v", err)
		return
	}
	outcome = telemetry.OutcomeSucceeded
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepPlatformLog, telemetry.Removed{Rows: n})
	if n > 0 {
		log.Printf("platform log prune: removed %d line(s) older than %d days",
			n, settings.PlatformLogRetentionDays)
	}
}

// pruneIntegrationSends ages out the app send ledger.
//
// Two sweeps in one pass, on purpose. Whole rows go at the administrator's
// integrations.retention_days (365 by default, never below 30 — below that the
// log stops being able to answer an incident). Retained message bodies go at 30
// days regardless of that setting: a body is a debugging aid for a live
// problem, not a year-long archive of every payload the install has
// transmitted. PruneSends does both, so the two cannot drift apart.
//
// Read per pass rather than at startup, so an administrator who shortens
// retention does not have to wait for a restart to see it applied.
func pruneIntegrationSends(db *database.DB) {
	t := time.NewTicker(appEventPruneInterval)
	defer t.Stop()
	for {
		pruneIntegrationSendsPass(db)
		<-t.C
	}
}

func pruneIntegrationSendsPass(db *database.DB) {
	ctx, span := telemetry.Start(context.Background(), "integrations.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepIntegrations, outcome, time.Since(start))
	}()

	settings, err := db.GetIntegrationSettings()
	if err != nil {
		log.Printf("integration send prune: reading settings: %v", err)
		return
	}
	n, cutoff, err := db.PruneSends(settings.RetentionDays)
	if !cutoff.IsZero() {
		span.Set(telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))
	}
	if err != nil {
		log.Printf("integration send prune: %v", err)
		return
	}
	outcome = telemetry.OutcomeSucceeded
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepIntegrations, telemetry.Removed{Rows: n})
	if n > 0 {
		log.Printf("integration send prune: removed %d send(s) older than %d days",
			n, settings.RetentionDays)
	}
}

// pruneAnnouncementRecipients ages out the list of who each announcement
// reached, on the administrator's announcement_recipient_retention_days.
//
// **Two facts with different lifetimes, and this sweep is where they part.**
// "On the 3rd of March this install told 214 people the queue was down, and 12
// sends failed" is the install's own record; "we mailed bob@example.com on the
// 3rd of March" is a named person's data with a shorter honest answer. The
// announcement row and its five counts are never pruned at all.
//
// The three disciplines are the events prune's and are held for the same
// reasons: read per pass so a shortened retention does not wait for a restart,
// skip the pass rather than guessing a cutoff when the settings read fails, and
// its own sweep name so two clocks are not one line in the operator's metric.
func pruneAnnouncementRecipients(db *database.DB) {
	t := time.NewTicker(appEventPruneInterval)
	defer t.Stop()
	for {
		pruneAnnouncementRecipientsPass(db)
		pruneReportDeliveryRecipientsPass(db)
		<-t.C
	}
}

// The report deliveries' per-person rows go on the same clock and in the same
// pass, because they answer the same question about the same install: how long
// do we keep a record of who we mailed. A second sweep with a second setting
// would be two answers to one question.
func pruneReportDeliveryRecipientsPass(db *database.DB) {
	settings, err := db.GetPlatformSettings()
	if err != nil {
		log.Printf("report delivery sweep: reading settings: %v", err)
		return
	}
	n, _, err := db.PruneReportDeliveryRecipients(settings.AnnouncementRecipientRetentionDays)
	if err != nil {
		log.Printf("report delivery sweep: %v", err)
		return
	}
	if n > 0 {
		log.Printf("report delivery sweep: removed %d recipient row(s)", n)
	}
}

func pruneAnnouncementRecipientsPass(db *database.DB) {
	ctx, span := telemetry.Start(context.Background(), "announcements.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepAnnouncementRecipients, outcome, time.Since(start))
	}()

	settings, err := db.GetPlatformSettings()
	if err != nil {
		log.Printf("announcement recipient prune: reading settings: %v", err)
		return
	}
	n, cutoff, err := db.PruneAnnouncementRecipients(settings.AnnouncementRecipientRetentionDays)
	if !cutoff.IsZero() {
		span.Set(telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))
	}
	if err != nil {
		log.Printf("announcement recipient prune: %v", err)
		return
	}
	outcome = telemetry.OutcomeSucceeded
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepAnnouncementRecipients,
		telemetry.Removed{Rows: int64(n)})
	if n > 0 {
		log.Printf("announcement recipient prune: removed %d recipient row(s) from announcements "+
			"completed more than %d days ago", n, settings.AnnouncementRecipientRetentionDays)
	}
}

// openIsolatedBackend builds the configured cluster backend, or nil.
//
// Nil is a supported state, not a degraded one: a VM install has nowhere to
// put an isolated workload, and the whole design is that absent configuration
// means the capability is simply not offered.
//
// It returns the concrete backend rather than one of the interfaces it
// satisfies because the caller needs three of them — services, jobs, and
// storage bindings — and they are all the same cluster.
func openIsolatedBackend(cfg *config.Config) *kube.Backend {
	if !cfg.KubernetesConfigured() {
		// The ECS backend is not implemented; see docs/work-remaining.md §2.
		// Reporting that here beats constructing something that fails later.
		if cfg.ECSConfigured() {
			log.Printf("WARNING: ECS_CLUSTER is set but the ECS backend is not implemented — " +
				"isolated mode is unavailable on this install.")
		}
		return nil
	}

	// The isolated half of BLOCK_AWS_METADATA. The shared half is an iptables
	// rule installed by the entrypoint; a pod is not covered by it, and on a
	// self-managed cluster the CSI drivers authenticate as the *node*, so an
	// app pod that could reach the metadata endpoint could take that identity
	// and read the storage its mounts were meant to mediate.
	var blockedEgress []string
	if cfg.BlockAWSMetadata {
		blockedEgress = kube.MetadataCIDRs
	}

	// Strict, like SHARED_STORAGE_ROOTS: a class list that did not parse is a
	// drive nobody can pick, discovered at the first create that wanted it.
	extraClasses, err := runtime.ParseStorageClasses(cfg.KubeStorageClasses)
	if err != nil {
		fatalf("KUBE_STORAGE_CLASSES: %v", err)
	}

	backend, err := kube.New(kube.Config{
		Namespace:           cfg.KubeNamespace,
		KubeConfig:          cfg.KubeConfig,
		Context:             cfg.KubeContext,
		CallbackURL:         cfg.CallbackAddr(),
		RunnerImages:        cfg.RunnerImages,
		StorageClass:        cfg.KubeStorageClass,
		S3FilesStorageClass: cfg.KubeS3FilesStorageClass,
		ExtraStorageClasses: extraClasses,
		StoreCSIDriver:      cfg.KubeStoreCSIDriver,
		StoreAccount:        cfg.StoreAccount,
		StoreRegion:         cfg.KubeStoreRegion,
		BlockedEgressCIDRs:  blockedEgress,
	})
	if err != nil {
		log.Printf("WARNING: the Kubernetes backend could not be opened, so isolated mode "+
			"is unavailable: %v", err)
		return nil
	}
	return backend
}

type namedServer struct {
	name string
	srv  *http.Server
}

// drainServers stops the listeners accepting and waits for in-flight handlers
// to return, concurrently and under one shared deadline. A server still
// serving at the deadline is closed outright: that cuts off the tail rather
// than every connection, which is the whole difference between a pause and an
// outage. Nil entries are skipped, so a single-listener install needs no
// special case.
func drainServers(timeout time.Duration, servers ...namedServer) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, s := range servers {
		if s.srv == nil {
			continue
		}
		wg.Add(1)
		go func(s namedServer) {
			defer wg.Done()
			if err := s.srv.Shutdown(ctx); err != nil {
				log.Printf("%s did not drain within %s: %v — closing", s.name, timeout, err)
				s.srv.Close()
			}
		}(s)
	}
	wg.Wait()
}

// openArtifactStore builds the configured store, or nil when none is
// configured. Nil is a supported state, not a degraded one: a VM install needs
// no object store, registry, or cloud account, and nothing about how a release
// starts changes without one.
//
// A release is packed on the disk its tree is on before it is uploaded, never in
// a TMPDIR that is a small tmpfs on some hosts and a container's own layer on
// others. builder's `.staging` is the same directory a reader's fetch uses.
func artifactStaging(cfg *config.Config) string {
	dir := filepath.Join(cfg.AppsDir, ".staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("artifact store: packing in TMPDIR, %s is unusable: %v", dir, err)
		return ""
	}
	return dir
}

func openArtifactStore(cfg *config.Config) (artifact.Store, error) {
	switch cfg.ArtifactStore {
	case "":
		return nil, nil

	case "local":
		store, err := artifact.NewLocalStore(cfg.ArtifactDir)
		if err != nil {
			return nil, err
		}
		return store, nil

	case "s3":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		store, err := artifact.NewS3Store(ctx, artifact.S3Config{
			Bucket:       cfg.ArtifactBucket,
			Prefix:       cfg.ArtifactPrefix,
			Endpoint:     cfg.ArtifactEndpoint,
			Region:       cfg.ArtifactRegion,
			UsePathStyle: cfg.ArtifactS3PathStyle,
			StagingDir:   artifactStaging(cfg),
		})
		if err != nil {
			return nil, err
		}
		return store, nil

	case "azblob":
		// No context and no timeout: the Blob client is built from a URL and a
		// credential chain and reaches nothing until the first call, unlike the
		// AWS loader above, which resolves credentials as it is constructed.
		store, err := artifact.NewAzBlobStore(artifact.AzBlobConfig{
			Account:          cfg.ArtifactAccount,
			Container:        cfg.ArtifactBucket,
			Prefix:           cfg.ArtifactPrefix,
			Endpoint:         cfg.ArtifactEndpoint,
			ConnectionString: cfg.ArtifactConnectionString,
			StagingDir:       artifactStaging(cfg),
		})
		if err != nil {
			return nil, err
		}
		return store, nil

	default:
		// config.Load already rejects anything else; this keeps the switch
		// exhaustive rather than silently disabling the capability.
		return nil, fmt.Errorf("unknown artifact store %q", cfg.ArtifactStore)
	}
}

// openStoreSigner builds the signer STORE_BACKEND names.
//
// A backend, never a cloud, and never derived from ARTIFACT_STORE: the artifact
// store is the platform's own bucket and a store is somebody else's, so an
// install may sign against one account and publish to another (ADR §82).
func openStoreSigner(cfg *config.Config) (artifact.Signer, error) {
	switch cfg.StoreBackend {
	case "azblob":
		// No timeout: the Blob client reaches nothing until the first call. The
		// user delegation key it signs with is fetched lazily and cached, so an
		// install whose identity is not yet granted fails at the first presign
		// rather than at boot — which is the same place an S3 permission
		// problem surfaces.
		return artifact.NewAzBlobSigner(artifact.AzBlobSignerConfig{
			Account:          cfg.StoreAccount,
			Endpoint:         cfg.StoreEndpoint,
			ConnectionString: cfg.StoreConnectionString,
		})
	default:
		// config.Load already rejects anything but the two, so this is "s3".
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return artifact.NewS3Signer(ctx, artifact.S3SignerConfig{
			Endpoint:     cfg.StoreEndpoint,
			Region:       cfg.StoreRegion,
			UsePathStyle: cfg.StoreS3PathStyle,
		})
	}
}

// logSignInBootstrap reports how someone is expected to sign in, and warns when
// the answer is "nobody can".
//
// Providers live in the database now, which is the right place for something an
// administrator changes — but it also means a correctly configured, healthy
// install can have no way in at all, and nothing about the process would look
// wrong. This is the one line that says so.
func logSignInBootstrap(cfg *config.Config, db *database.DB) {
	var ways []string
	if cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "" {
		ways = append(ways, "GitHub (from GITHUB_CLIENT_ID)")
	}
	if cfg.OIDCIssuer != "" && cfg.OIDCClientID != "" {
		ways = append(ways, fmt.Sprintf("%s (from OIDC_ISSUER)", cfg.OIDCIssuer))
	}

	// Only enabled rows count: a provider an administrator switched off is not
	// a way in, and reporting it as one would hide the problem this warning
	// exists to surface.
	if providers, err := db.ListAuthProviders(); err != nil {
		log.Printf("Could not list sign-in providers: %v", err)
		return
	} else {
		for _, p := range providers {
			if p.Enabled {
				ways = append(ways, fmt.Sprintf("%s (%s, configured by an admin)", p.DisplayName, p.Kind))
			}
		}
	}

	if len(ways) == 0 {
		// Reachable: an administrator may switch local sign-in off, and an
		// environment-configured provider can then be removed from the environment.
		log.Printf("WARNING: no sign-in provider is enabled — nobody can log in. " +
			"Set OIDC_ISSUER + OIDC_CLIENT_ID + OIDC_CLIENT_SECRET (any OpenID Connect provider), " +
			"or GITHUB_CLIENT_ID + GITHUB_CLIENT_SECRET, or run `toft local-admin enable` " +
			"to switch the built-in local administrator back on.")
		return
	}
	log.Printf("Sign-in: %s", strings.Join(ways, ", "))

	// An install still holding the published default password has an administrator
	// account that belongs to whoever reaches the login page first. Said loudly,
	// every boot, until it is dealt with — this is the one state where the
	// bootstrap account is a liability rather than a convenience.
	if isDefault, err := db.BuiltinAdminHasDefaultPassword(); err != nil {
		log.Printf("Could not check the built-in administrator's password: %v", err)
	} else if isDefault && localSignInEnabled(db) {
		log.Printf("**********************************************************************")
		log.Printf("*  The built-in administrator (%q) still has the published default", database.BuiltinAdminLogin)
		log.Printf("*  password. It cannot mint a session — the first sign-in must replace")
		log.Printf("*  it — but whoever reaches the login page first is the one who")
		log.Printf("*  chooses. Claim it now, or set a password of your own with:")
		log.Printf("*      toft local-admin enable")
		log.Printf("**********************************************************************")
	}
}

// localSignInEnabled reports whether the built-in password door is open. Errors
// are treated as closed: the only caller is a warning, and warning about a door
// that is shut is worse than staying quiet.
func localSignInEnabled(db *database.DB) bool {
	p, err := db.GetAuthProvider(database.LocalProviderID)
	return err == nil && p.Enabled
}

// openHostStore builds the host data plane, or says why there is none.
func openHostStore(cfg *config.Config, roots []runtime.StorageRoot, db *database.DB) (*hoststore.Store, string) {
	switch {
	case len(roots) == 0:
		return nil, "no storage root is declared"
	case !cfg.AppUserPerApp:
		return nil, "apps here share one account (APP_USER_PER_APP is off), so a folder's group could not keep an app out"
	}
	if err := hoststore.Probe(); err != nil {
		log.Printf("WARNING: host storage access is off: %v", err)
		return nil, err.Error()
	}
	audit := func(c hoststore.Claims, n int64) {
		// The bytes passed through the platform, so the write is recorded as
		// landed — which a bucket's mint-only audit cannot say.
		if err := db.WriteAuditRow(database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: c.Subject,
			Action:     "store.write",
			TargetType: "storage_resource", TargetID: c.Resource,
			After: hostStoreAuditAfter(c.Key, n),
		}); err != nil {
			log.Printf("auditing a host store write: %v", err)
		}
	}
	log.Printf("Host storage access: enabled at %s", hoststore.Route)
	return hoststore.New(hoststore.DeriveKey([]byte(cfg.JWTSecret)), cfg.BaseURL, roots, audit), ""
}

// hostStoreAuditAfter is a host-store write's audit `after`, as JSON. Built by
// json.Marshal, never %q: Go quoting is not JSON escaping, and a name holding a
// rune such as U+E0001 came out as \U000e0001, which the jsonb column refused,
// so the landing row was never written (AS-7).
func hostStoreAuditAfter(key string, n int64) string {
	b, _ := json.Marshal(struct {
		Key   string `json:"key"`
		Bytes int64  `json:"bytes"`
	}{key, n})
	return string(b)
}

// fatalf and fatal end a start that cannot go on, at ERROR.
//
// After obs.Init the standard log package is bridged into the structured
// handler at INFO, so a log.Fatalf there was an INFO record: LOG_LEVEL=warn or
// error -- or the administrator's own stored level, applied before encryption
// is built -- dropped it, and the process exited 1 with no reason at all
// (docs/bugs 2026-10-22-e2e-install #1). ERROR is the one level no setting
// suppresses.
func fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

func fatal(args ...any) {
	slog.Error(fmt.Sprint(args...))
	os.Exit(1)
}

// buildAccountFor is the builder's account resolver: the app's own account where
// the install separates apps from the platform, and nil — build as the platform
// — where it does not.
func buildAccountFor(i runtime.Isolation) func(string) (*runtime.AppAccount, error) {
	if !i.Enabled() {
		return nil
	}
	return i.Account
}

// appOutputOption hands the server the output copy, or nothing when this
// install runs none.
func appOutputOption(o *appout.Sink) api.ServerOption {
	if o == nil {
		return func(*api.Server) {}
	}
	return api.WithAppOutput(o)
}
