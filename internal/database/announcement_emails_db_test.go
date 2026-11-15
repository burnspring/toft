package database_test

import (
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The announcement store. Every test here is about the record being true: the
// freeze happening once, the audience resolving the way the switch says, and
// the counts on the row equalling the rows underneath it.

// mkAnnouncementUser writes a user with the three facts the audience reads.
// Raw SQL rather than UpsertUser because locked_at and an empty address are the
// whole subject and neither is settable through the normal path.
func mkAnnouncementUser(t *testing.T, db *database.DB, login, role, email string, locked bool) string {
	t.Helper()
	id := "au-" + login
	var lockedAt any
	if locked {
		lockedAt = time.Now().UTC()
	}
	_, err := db.Conn().Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, system_role, created_at, locked_at)
		VALUES (?, ?, '', ?, '', ?, NOW(), ?)`, id, login, email, role, lockedAt)
	if err != nil {
		t.Fatalf("creating %s: %v", login, err)
	}
	return id
}

func mkDraft(t *testing.T, db *database.DB, audience string) *database.AnnouncementEmail {
	t.Helper()
	a := &database.AnnouncementEmail{
		Subject:  "The build queue is down",
		Body:     "Back at 14:00.",
		Audience: audience,
	}
	if err := db.CreateAnnouncementEmail(a); err != nil {
		t.Fatalf("CreateAnnouncementEmail: %v", err)
	}
	return a
}

// A regression here double-mails the install and nothing reports it: the second
// send would look, in every log and on every screen, exactly like the first.
func TestSendIsGuardedByStatus(t *testing.T) {
	db := dbtest.New(t)
	a := mkDraft(t, db, database.AnnouncementAudienceAll)

	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := db.BeginAnnouncementSend(a.ID, "")
			if err != nil {
				t.Errorf("BeginAnnouncementSend: %v", err)
			}
			won[i] = ok
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d concurrent sends froze the draft; exactly one may", winners, racers)
	}

	got, err := db.GetAnnouncementEmail(a.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.Status != database.AnnouncementSending {
		t.Errorf("status = %q, want %q", got.Status, database.AnnouncementSending)
	}
	if got.SentAt == nil {
		t.Error("sent_at was not stamped by the freeze")
	}
}

// The two audiences, and the two skip reasons that are the whole of the list.
func TestAudienceResolution(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)
	mkAnnouncementUser(t, db, "bob", database.SystemRoleViewer, "bob@example.com", false)
	mkAnnouncementUser(t, db, "cleo", database.SystemRoleAdmin, "cleo@example.com", false)
	mkAnnouncementUser(t, db, "dev", database.SystemRolePublisher, "dev@example.com", true)
	mkAnnouncementUser(t, db, "eve", database.SystemRolePublisher, "", false)

	// The baseline seeds `admin` — a system_admin with an address — so it is
	// in both audiences and reachable in both. Named here rather than worked
	// around: the built-in administrator is a person this install can tell
	// things to, and an audience that quietly excluded it would be wrong.
	all := mkDraft(t, db, database.AnnouncementAudienceAll)
	pending, err := db.ResolveAnnouncementRecipients(all.ID, all.Audience)
	if err != nil {
		t.Fatalf("resolving all: %v", err)
	}
	if got := logins(pending); len(got) != 4 {
		t.Errorf("audience all reached %v, want ada, bob, cleo and admin", got)
	}

	pubs := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	pending, err = db.ResolveAnnouncementRecipients(pubs.ID, pubs.Audience)
	if err != nil {
		t.Fatalf("resolving publishers: %v", err)
	}
	got := logins(pending)
	if len(got) != 3 || !got["ada"] || !got["cleo"] || !got["admin"] {
		t.Errorf("audience publishers reached %v, want ada, cleo and admin — an administrator is a publisher", got)
	}
	if got["bob"] {
		t.Error("a viewer was mailed an announcement addressed to publishers")
	}

	// The skips are rows with reasons, never decrements.
	rows, err := db.ListAnnouncementRecipients(pubs.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	reasons := map[string]string{}
	for _, r := range rows {
		if r.Status == database.AnnouncementSkipped {
			reasons[r.Login] = r.Reason
		}
	}
	if reasons["dev"] != database.AnnouncementSkipLocked {
		t.Errorf("locked user skipped for %q, want %q", reasons["dev"], database.AnnouncementSkipLocked)
	}
	if reasons["eve"] != database.AnnouncementSkipNoAddress {
		t.Errorf("address-less user skipped for %q, want %q", reasons["eve"], database.AnnouncementSkipNoAddress)
	}
	if len(rows) != 5 {
		t.Errorf("%d recipient rows for five publishers; a skip is a row, not a decrement", len(rows))
	}

	frozen, err := db.GetAnnouncementEmail(pubs.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if frozen.Resolved != 5 || frozen.Skipped != 2 {
		t.Errorf("resolved=%d skipped=%d, want 5 and 2", frozen.Resolved, frozen.Skipped)
	}
}

func logins(rs []database.AnnouncementRecipient) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		out[r.Login] = true
	}
	return out
}

// The assertion that the record is not lying. A drift between the counts and
// the rows is invisible on every surface.
func TestCountsMatchRecipientRows(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)
	mkAnnouncementUser(t, db, "bob", database.SystemRolePublisher, "bob@example.com", false)
	mkAnnouncementUser(t, db, "cleo", database.SystemRolePublisher, "cleo@example.com", false)
	mkAnnouncementUser(t, db, "dev", database.SystemRolePublisher, "", false)

	a := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	if _, err := db.BeginAnnouncementSend(a.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	pending, err := db.ResolveAnnouncementRecipients(a.ID, a.Audience)
	if err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}
	// ada, bob, cleo and the seeded admin. dev has no address and is a skip.
	if len(pending) != 4 {
		t.Fatalf("%d pending, want 4", len(pending))
	}

	// One delivered, one failed, one left pending for the abandon below.
	if err := db.CompleteAnnouncementRecipient(pending[0].ID, database.AnnouncementDelivered, "", 1); err != nil {
		t.Fatalf("completing: %v", err)
	}
	if err := db.CompleteAnnouncementRecipient(pending[1].ID, database.AnnouncementFailed, "relay said no", 3); err != nil {
		t.Fatalf("completing: %v", err)
	}
	if err := db.RecountAnnouncementEmail(a.ID); err != nil {
		t.Fatalf("RecountAnnouncementEmail: %v", err)
	}
	assertCountsMatchRows(t, db, a.ID)

	n, err := db.AbandonAnnouncementSend(a.ID)
	if err != nil {
		t.Fatalf("AbandonAnnouncementSend: %v", err)
	}
	if n != 2 {
		t.Errorf("abandoned %d, want 2", n)
	}
	assertCountsMatchRows(t, db, a.ID)

	got, err := db.GetAnnouncementEmail(a.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.Status != database.AnnouncementSent {
		t.Errorf("status = %q, want %q — sent is terminal whatever the outcomes were", got.Status, database.AnnouncementSent)
	}
	if got.Delivered != 1 || got.Failed != 1 || got.Skipped != 1 || got.Abandoned != 2 || got.Resolved != 5 {
		t.Errorf("counts = %+v, want resolved 5, delivered 1, failed 1, skipped 1, abandoned 2", got)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at was not stamped")
	}
}

func assertCountsMatchRows(t *testing.T, db *database.DB, id string) {
	t.Helper()
	a, err := db.GetAnnouncementEmail(id)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	rows, err := db.ListAnnouncementRecipients(id)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	byStatus := map[string]int{}
	for _, r := range rows {
		byStatus[r.Status]++
	}
	for _, c := range []struct {
		status string
		on     int
	}{
		{database.AnnouncementDelivered, a.Delivered},
		{database.AnnouncementFailed, a.Failed},
		{database.AnnouncementSkipped, a.Skipped},
		{database.AnnouncementAbandoned, a.Abandoned},
	} {
		if byStatus[c.status] != c.on {
			t.Errorf("%s: row says %d, recipients say %d", c.status, c.on, byStatus[c.status])
		}
	}
	if a.Resolved != len(rows) {
		t.Errorf("resolved = %d, %d recipient rows", a.Resolved, len(rows))
	}
}

// A restart abandons; it never resumes and never claims.
func TestAbandonSweepCompletesRow(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)
	mkAnnouncementUser(t, db, "bob", database.SystemRolePublisher, "bob@example.com", false)

	a := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	if _, err := db.BeginAnnouncementSend(a.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	pending, err := db.ResolveAnnouncementRecipients(a.ID, a.Audience)
	if err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}
	if err := db.CompleteAnnouncementRecipient(pending[0].ID, database.AnnouncementDelivered, "", 1); err != nil {
		t.Fatalf("completing: %v", err)
	}

	announcements, recipients, err := db.AbandonStrandedAnnouncements()
	if err != nil {
		t.Fatalf("AbandonStrandedAnnouncements: %v", err)
	}
	if announcements != 1 || recipients != 2 {
		t.Fatalf("swept %d announcement(s) and %d recipient(s), want 1 and 2", announcements, recipients)
	}

	got, err := db.GetAnnouncementEmail(a.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.Status != database.AnnouncementSent || got.Abandoned != 2 || got.Delivered != 1 {
		t.Errorf("after the sweep: %q delivered=%d abandoned=%d", got.Status, got.Delivered, got.Abandoned)
	}

	// The delivered row is untouched: nothing is re-sent, and nothing already
	// recorded is rewritten.
	rows, err := db.ListAnnouncementRecipients(a.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	for _, r := range rows {
		if r.ID == pending[0].ID && r.Status != database.AnnouncementDelivered {
			t.Errorf("the delivered recipient became %q", r.Status)
		}
	}

	// A second sweep finds nothing: the row is no longer sending.
	announcements, _, err = db.AbandonStrandedAnnouncements()
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if announcements != 0 {
		t.Errorf("the sweep found %d stranded announcement(s) on a second pass", announcements)
	}
}

// A frozen row refuses edits. Deletion is the other way round: a run in flight
// refuses it and a completed record accepts it. The store half of both; the
// handlers turn the false into a 409.
func TestFrozenRowRefusesEdits(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)
	a := mkDraft(t, db, database.AnnouncementAudienceAll)

	a.Subject = "Rewritten"
	ok, err := db.UpdateAnnouncementDraft(a)
	if err != nil || !ok {
		t.Fatalf("a draft refused an edit: ok=%v err=%v", ok, err)
	}

	if _, err := db.BeginAnnouncementSend(a.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	if _, err := db.ResolveAnnouncementRecipients(a.ID, a.Audience); err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}
	a.Subject = "Rewritten after the freeze"
	if ok, err := db.UpdateAnnouncementDraft(a); err != nil || ok {
		t.Errorf("a sending row accepted an edit: ok=%v err=%v", ok, err)
	}
	// The one delete that is refused, and it is refused because the runner is
	// still writing into this id rather than because of what the row means.
	if ok, err := db.DeleteAnnouncementEmail(a.ID); err != nil || ok {
		t.Errorf("a sending row was deleted: ok=%v err=%v", ok, err)
	}

	if err := db.FinishAnnouncementSend(a.ID); err != nil {
		t.Fatalf("FinishAnnouncementSend: %v", err)
	}
	got, err := db.GetAnnouncementEmail(a.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.Subject != "Rewritten" {
		t.Errorf("subject = %q; the freeze did not hold", got.Subject)
	}
	// And once the run has completed, the record is the administrator's to be
	// rid of — the recipient rows with it, by cascade.
	if ok, err := db.DeleteAnnouncementEmail(a.ID); err != nil || !ok {
		t.Errorf("a sent row refused deletion: ok=%v err=%v", ok, err)
	}
	if got, err := db.GetAnnouncementEmail(a.ID); err != nil || got != nil {
		t.Errorf("the record is still there: %v, err=%v", got, err)
	}
	rs, err := db.ListAnnouncementRecipients(a.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("%d recipient row(s) outlived the record they hang off", len(rs))
	}
}

// The estimate is what the composer shows, and it has to agree with what the
// resolution actually does — one definition of the audience, read twice.
func TestEstimateAgreesWithResolution(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)
	mkAnnouncementUser(t, db, "dev", database.SystemRolePublisher, "dev@example.com", true)
	mkAnnouncementUser(t, db, "eve", database.SystemRolePublisher, "", false)

	e, err := db.EstimateAnnouncementAudience(database.AnnouncementAudiencePublishers)
	if err != nil {
		t.Fatalf("EstimateAnnouncementAudience: %v", err)
	}
	// ada, dev, eve and the seeded admin.
	if e.Total != 4 || e.Deliverable != 2 || e.SkipLocked != 1 || e.SkipNoAddress != 1 {
		t.Fatalf("estimate = %+v, want 4/2/1/1", e)
	}

	a := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	pending, err := db.ResolveAnnouncementRecipients(a.ID, a.Audience)
	if err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}
	if len(pending) != e.Deliverable {
		t.Errorf("the estimate said %d deliverable and the resolution found %d", e.Deliverable, len(pending))
	}

	if _, err := db.EstimateAnnouncementAudience("everyone-i-like"); err == nil {
		t.Error("an unknown audience was estimated rather than refused")
	}
}

// The counts are permanent; the recipient rows are not. And a run still in
// flight is never eligible, because the cutoff is on the completion.
func TestPruneAnnouncementRecipients(t *testing.T) {
	db := dbtest.New(t)
	mkAnnouncementUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com", false)

	old := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	if _, err := db.BeginAnnouncementSend(old.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	if _, err := db.ResolveAnnouncementRecipients(old.ID, old.Audience); err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}
	if err := db.FinishAnnouncementSend(old.ID); err != nil {
		t.Fatalf("FinishAnnouncementSend: %v", err)
	}
	if _, err := db.Conn().Exec(
		`UPDATE announcement_emails SET completed_at = NOW() - INTERVAL '400 days' WHERE id = ?`,
		old.ID); err != nil {
		t.Fatalf("ageing the row: %v", err)
	}

	live := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	if _, err := db.BeginAnnouncementSend(live.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	if _, err := db.ResolveAnnouncementRecipients(live.ID, live.Audience); err != nil {
		t.Fatalf("ResolveAnnouncementRecipients: %v", err)
	}

	n, _, err := db.PruneAnnouncementRecipients(180)
	if err != nil {
		t.Fatalf("PruneAnnouncementRecipients: %v", err)
	}
	// ada and the seeded admin.
	if n != 2 {
		t.Fatalf("pruned %d recipient(s), want 2", n)
	}

	kept, err := db.GetAnnouncementEmail(old.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if kept.Resolved != 2 {
		t.Errorf("the counts were pruned with the recipients: resolved=%d", kept.Resolved)
	}
	rows, err := db.ListAnnouncementRecipients(live.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("a live send lost %d recipient row(s) to the prune", 2-len(rows))
	}
}

// Clone copies the content and the audience and nothing else. The store half:
// cloned_from carries no foreign key, so the source may be deleted afterwards.
func TestCloneCopiesContentNotRecord(t *testing.T) {
	db := dbtest.New(t)
	src := mkDraft(t, db, database.AnnouncementAudiencePublishers)
	if _, err := db.BeginAnnouncementSend(src.ID, ""); err != nil {
		t.Fatalf("BeginAnnouncementSend: %v", err)
	}
	if err := db.FinishAnnouncementSend(src.ID); err != nil {
		t.Fatalf("FinishAnnouncementSend: %v", err)
	}

	clone := &database.AnnouncementEmail{
		Subject: src.Subject, Body: src.Body, Audience: src.Audience, ClonedFrom: src.ID,
	}
	if err := db.CreateAnnouncementEmail(clone); err != nil {
		t.Fatalf("CreateAnnouncementEmail: %v", err)
	}
	got, err := db.GetAnnouncementEmail(clone.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.Status != database.AnnouncementDraft || got.SentAt != nil || got.Resolved != 0 {
		t.Errorf("the clone carried the record over: %+v", got)
	}
	if got.ClonedFrom != src.ID {
		t.Errorf("cloned_from = %q, want %q", got.ClonedFrom, src.ID)
	}

	if _, err := db.Conn().Exec(`DELETE FROM announcement_emails WHERE id = ?`, src.ID); err != nil {
		t.Fatalf("deleting the source: %v", err)
	}
	got, err = db.GetAnnouncementEmail(clone.ID)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	if got.ClonedFrom != src.ID {
		t.Errorf("cloned_from was cleared when the source went; it carries no foreign key for that reason")
	}
}
