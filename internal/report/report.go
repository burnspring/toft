// Package report delivers a rendered report to the people its owner named.
//
// A notebook re-renders on a schedule and then sits there: the install has the
// freshest possible answer to a question and no way to say so. This is the
// other half.
//
// **What arrives is a link, not the report.** An attachment is a copy of the
// data that leaves this install's authorization the moment it is sent, readable
// forever by anyone the mail reaches, with no revocation. A link is the current
// numbers behind the sign-in the app already has. That makes this narrower than
// what people describe when they ask for it, and the narrowing is the decision
// rather than a shortcut.
//
// The three rules it holds:
//
//   - **A recipient is re-checked, every send, through the authorizer.** Not
//     through `EffectiveRole`, which reads grants alone and would answer "" for
//     the app's own owner — the person who built the report would be the one
//     person who never received it. The authorizer is also the one definition of
//     readable apps, which is an invariant rather than a preference.
//   - **Somebody who has lost access is skipped with a reason, not removed.**
//     The list is the owner's intent and the delivery row is what happened;
//     collapsing them would let a week's outage quietly rewrite who a report is
//     for.
//   - **A run that sent nothing still writes a row.** "Did Monday's report go
//     out" has to be answerable, and silence is not an answer.
package report

import (
	"context"
	"fmt"
	"log"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/mail"
	"toft/internal/notify"
)

// Store is the slice of the database this package needs. An interface so the
// failure modes — a recipient who lost access, a floor that suppresses, a relay
// that refuses one address — are reachable without a Postgres.
type Store interface {
	GetRenderSchedule(appID string) (*database.RenderSchedule, error)
	ListScheduleRecipients(scheduleID string) ([]database.ScheduleRecipient, error)
	LastReportDeliveryAt(scheduleID string) (*time.Time, error)
	RecordReportDelivery(ctx context.Context, d *database.ReportDelivery) error
	PrincipalForUser(userID string) (*auth.Principal, error)
	GetUser(id string) (*database.User, error)
	GetPlatformSettings() (database.PlatformSettings, error)
}

// Mailer is the one verb this package needs from the mail service. Nil on an
// install with no relay, which is not an error: every recipient is skipped with
// that reason and the row says so.
type Mailer interface {
	SendOne(ctx context.Context, msg mail.Message) error
}

// Deliverer announces a render to a schedule's recipients.
type Deliverer struct {
	store  Store
	mailer Mailer
	// resolver answers the authorization question. Held as a field so a test
	// can drive access changes without building a grant graph.
	resolver auth.RoleResolver
	now      func() time.Time
}

func New(store Store, mailer Mailer, resolver auth.RoleResolver) *Deliverer {
	return &Deliverer{store: store, mailer: mailer, resolver: resolver, now: func() time.Time {
		return time.Now().UTC()
	}}
}

// Deliver announces one successful render.
//
// **It never returns an error that matters to its caller**, and the caller
// treats it that way: the deploy has already succeeded and the release is
// already serving by the time this runs. A send that could fail a deploy would
// make a mail relay a dependency of publishing.
func (d *Deliverer) Deliver(ctx context.Context, app *database.App, deploymentID, appURL string) {
	if app == nil || d.store == nil {
		return
	}
	sch, err := d.store.GetRenderSchedule(app.ID)
	if err != nil || sch == nil {
		if err != nil {
			log.Printf("report: reading the schedule for %s: %v", app.Slug, err)
		}
		return
	}
	recipients, err := d.store.ListScheduleRecipients(sch.ID)
	if err != nil {
		log.Printf("report: reading recipients for %s: %v", app.Slug, err)
		return
	}
	// An empty list is not an error and not a delivery. The schedule is a
	// re-render exactly as it was before anybody attached a list, and writing a
	// row for it would fill the record with runs nobody asked to be told about.
	if len(recipients) == 0 {
		return
	}

	delivery := &database.ReportDelivery{
		AppID: app.ID, ScheduleID: sch.ID, DeploymentID: deploymentID,
		CreatedAt: d.now(),
	}

	if reason, suppressed := d.tooSoon(sch.ID); suppressed {
		// Every recipient carries the reason rather than the run carrying one
		// summary: the row is read per person, and "why did I not get Monday's"
		// is a question about one person.
		for _, r := range recipients {
			delivery.Recipients = append(delivery.Recipients, database.ReportDeliveryRecipient{
				UserID: r.UserID, Login: r.Login, Email: r.Email,
				Status: database.DeliverySkipped, Reason: reason,
			})
		}
		d.record(ctx, app, delivery)
		return
	}

	for _, r := range recipients {
		row := database.ReportDeliveryRecipient{
			UserID: r.UserID, Login: r.Login, Email: r.Email,
		}
		switch {
		case !d.mayRead(app, r.UserID):
			// The link would refuse them, and a mail whose only content is a
			// link the reader cannot open is worse than silence.
			row.Status, row.Reason = database.DeliverySkipped, d.whySkipped(app, r.UserID)
		case r.Email == "":
			// Named back rather than logged, for the reason the share notice
			// already names it back: the directory picker carries no address, so
			// this is discoverable nowhere else.
			row.Status, row.Reason = database.DeliverySkipped, "no address on file"
		case d.mailer == nil:
			row.Status, row.Reason = database.DeliverySkipped, "no mail relay is configured"
		default:
			row.Status, row.Reason = d.send(ctx, app, r, appURL)
		}
		delivery.Recipients = append(delivery.Recipients, row)
	}
	d.record(ctx, app, delivery)
}

// tooSoon reports whether the floor suppresses this run.
//
// Measured from the last *attempt* rather than the last successful send: a run
// the floor already suppressed still wrote a row, so measuring from rows is what
// stops a minutely schedule resetting its own clock — and measuring from
// successes would let one broken relay switch the floor off.
func (d *Deliverer) tooSoon(scheduleID string) (string, bool) {
	settings, err := d.store.GetPlatformSettings()
	if err != nil {
		// A settings read that fails is not a license to mail at any rate. The
		// safe reading of an unknown floor is the default one.
		log.Printf("report: reading settings: %v", err)
		return "", false
	}
	floor := time.Duration(settings.ReportDeliveryMinIntervalMinutes) * time.Minute
	if floor <= 0 {
		return "", false
	}
	last, err := d.store.LastReportDeliveryAt(scheduleID)
	if err != nil || last == nil {
		return "", false
	}
	if elapsed := d.now().Sub(*last); elapsed < floor {
		return fmt.Sprintf("this report was already delivered %s ago, and the install allows one every %s",
			elapsed.Round(time.Minute), floor), true
	}
	return "", false
}

// mayRead is the authorization question, asked per recipient per send.
//
// `auth.Authorize` rather than `EffectiveRole`: the second reads `object_grants`
// alone, so it answers "" for the app's own owner and for every account on an
// app whose visibility is `authenticated`. A list filtered through it would skip
// the person who built the report, and it would look like they had mistyped
// their own name into the picker.
//
// **The lock is checked separately, and it has to be.** A lock is enforced at
// sign-in and at the door, not in the authorizer — an `auth.Principal` has no
// field for it, because one only exists for somebody who already got in. This
// caller is different: it builds a principal for a person who is not here, so
// the question the authorizer answers ("may this account read this app") is not
// the question being asked ("should we mail this person"). Without this, a
// report would keep arriving weekly for somebody the directory deactivated —
// which is exactly the offboarding hole the SCIM feed closes on the
// authentication side, reopened on the mail side.
func (d *Deliverer) mayRead(app *database.App, userID string) bool {
	user, err := d.store.GetUser(userID)
	if err != nil || user == nil || user.Locked() {
		return false
	}
	p, err := d.store.PrincipalForUser(userID)
	if err != nil || p == nil {
		return false
	}
	return auth.Authorize(p, auth.ActionRead, app.AuthObject(), d.resolver).Allow
}

func (d *Deliverer) send(ctx context.Context, app *database.App, r database.ScheduleRecipient, appURL string) (string, string) {
	msg, err := mail.Render("report_rendered", notify.TemplateData{
		AppName: app.Name,
		// The app's own address, passed in already built. Every other link in
		// this product's mail points at the platform and resolves through the
		// mail service's base URL; a report's points at the apps origin, which
		// on a split-origin install is a different host.
		Link: appURL,
	})
	if err != nil {
		return database.DeliveryFailed, "the message could not be built: " + err.Error()
	}
	msg.To = []string{r.Email}
	if err := d.mailer.SendOne(ctx, msg); err != nil {
		// The relay's own words. One recipient failing does not stop the rest:
		// a rate limit that refuses the fortieth address is not a reason the
		// first thirty-nine should not have gone.
		return database.DeliveryFailed, err.Error()
	}
	return database.DeliveryDelivered, ""
}

func (d *Deliverer) record(ctx context.Context, app *database.App, delivery *database.ReportDelivery) {
	// Without the caller's deadline: a relay that stalled every send has spent
	// it, and the row saying so is the one worth having (PB-8).
	if err := d.store.RecordReportDelivery(context.WithoutCancel(ctx), delivery); err != nil {
		log.Printf("report: recording the delivery for %s: %v", app.Slug, err)
	}
}

// whySkipped names the reason a recipient did not qualify, so the row is worth
// reading. A locked account and a removed grant are different facts and the
// person asking why Ana stopped receiving the Monday report needs the right one.
func (d *Deliverer) whySkipped(app *database.App, userID string) string {
	user, err := d.store.GetUser(userID)
	if err != nil || user == nil {
		return "this account no longer exists"
	}
	if user.Locked() {
		return "this account is locked"
	}
	return "access to this app was removed"
}
