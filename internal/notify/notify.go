// Package notify holds the vocabulary every notification channel shares: who a
// message is for, and what a template may say about it.
//
// It exists because there are now two channels — email and Slack — and exactly
// two things had to be common between them. The audience names, because an
// administrator's "stop filling my inbox" and "stop telling my users things"
// are decisions about *messages*, not about transports, and a switch that meant
// something different in each channel would be a switch nobody could reason
// about. And the template data, because the two channels render the same
// notification and must not drift into knowing different facts about it.
//
// It holds nothing else, and deliberately no interface. There is no Channel
// abstraction here to implement: mail renders MIME and takes recipients, Slack
// renders blocks and takes a channel, and the seam that would hide that
// difference would have to invent a lowest common denominator neither of them
// wants. What is shared is the vocabulary, not the mechanism.
//
// mail aliases these names so no call site had to change when they moved here;
// slack imports this package rather than mail, because a Slack transport that
// imports the mailer to learn what an audience is has the dependency backwards.
package notify

// Audiences. Every message belongs to exactly one, and an administrator can
// switch either off per channel without switching the channel off: the two are
// wanted by different people for different reasons.
const (
	// AudienceAdmin is for the people who run the install, about the install:
	// the publisher-request queue and anything else that is an administrator's
	// job to act on.
	AudienceAdmin = "admin"
	// AudienceApp is for the people who use it, about their own apps and their
	// own access.
	AudienceApp = "app"
	// AudienceAppOutbound is a message an *app* wrote and asked the platform to
	// send: a report, an ingest summary, an alert only the app knows about.
	//
	// Its own audience, not AudienceApp. That switch means "stop sending app
	// authors platform chatter about their apps", and an administrator who
	// throws it is not asking to shut down a business process that mails a
	// finance team every Monday. Reusing it would make one switch mean two
	// things, and the first person to discover that would discover it as
	// silence.
	//
	// It has no switch in this file's vocabulary at all, and that is not a gap:
	// the rule "every audience has exactly one enforcement point" still holds,
	// and this one's is appint.Check — the install-wide integrations.email /
	// integrations.slack switch plus the per-app attachment. A send that has
	// not passed those gates never reaches a transport.
	AudienceAppOutbound = "app_outbound"
	// AudienceAnnouncement is the message an administrator writes and sends to
	// the people on this install: the first mail in the product whose arrival
	// is the point.
	//
	// **No switch silences it, and that is the decision rather than an
	// oversight.** `notify_app` means "stop telling app authors platform
	// chatter about their apps". `notify_admin` means "stop filling my inbox".
	// Neither of them means "stop the person who runs this install from
	// reaching the people on it", and routing an announcement through either
	// would produce the one lie its frozen record cannot afford: a row reading
	// *sent, 0 delivered* because a switch nobody associated with this feature
	// was off.
	//
	// The rule "every audience has exactly one enforcement point" still holds,
	// as it does for AudienceAppOutbound above. This one's is the Send button:
	// an announcement does not exist until an administrator writes it and does
	// not go out until the same administrator sends it, which is a stronger
	// switch than a checkbox and one nobody can leave off by accident.
	AudienceAnnouncement = "announcement"
	// AudienceReport is a rendered report reaching the people its owner named:
	// the **second** message in this product whose arrival is the point.
	//
	// Not AudienceApp, for AudienceAppOutbound's reason said again. That switch
	// means "stop sending app authors platform chatter about their apps", and an
	// administrator who throws it is not asking to stop a Monday report reaching
	// the people who read it. One switch meaning two things is discovered as
	// silence, by the person who configured the thing that went quiet.
	//
	// Not AudienceAppOutbound either, which is a message an *app* wrote and is
	// gated by `appint.Check` — an app wrote nothing here, and there is no
	// integration attached.
	//
	// **Its enforcement point is the recipient list**, and the rule that every
	// audience has exactly one still holds. A report does not exist until
	// somebody writes a cron *and* names people, and a list with nobody on it
	// sends nothing — the announcement's argument, *a stronger switch than a
	// checkbox and one nobody can leave off by accident*, applied to a row
	// rather than to a button.
	AudienceReport = "report"
	// AudienceTest is an administrator's own diagnostic send. It belongs to no
	// switch — a test button that goes quiet because notifications are off
	// tests nothing and reads as a broken relay.
	AudienceTest = "test"
)

// TemplateData is everything any notification template may reference. Fields
// not relevant to a given message are simply empty.
//
// One struct for both channels on purpose. A Slack post and an email about the
// same event are the same notification twice, and giving each its own payload
// is how they end up quoting different summaries or linking to different
// installs.
type TemplateData struct {
	BaseURL string
	// Requester identity, as displayed. DisplayName falls back to the login,
	// so no template has to decide what to do with a provider that asserts no
	// name.
	RequesterName  string
	RequesterLogin string
	RequesterEmail string
	// ApproverName is who decided, for the requester-facing messages.
	ApproverName string
	// Reason is the administrator's own words on a denial.
	Reason string
	// Link is the deep link into the platform. It is a pointer, not a
	// credential: whoever follows it still has to sign in and still has to be
	// authorized.
	Link string
	// Via names what actually carried the message — "your SMTP relay at
	// smtp.example.com", or "Resend". Only the test email uses it, and it uses
	// it because "we reached your relay" is the wrong thing to tell someone who
	// configured a sending API.
	Via string
	// AppName is the app something happened to, as its people call it.
	AppName string
	// EventTitle is the one-line summary of what broke, and doubles as the
	// subject line. EventDetail is the excerpt that says why — a build-log
	// tail, git's own stderr, an HTTP status — and may be empty.
	EventTitle  string
	EventDetail string
	// EventWhen is when it happened, already formatted: the reader needs to
	// know whether this is the thing they are looking at or something from
	// three hours ago.
	EventWhen string
	// Severity is "error", "warning" or "info" (good news: a job with notify =
	// "always" that succeeded), used only to pick words.
	Severity string
	// Role is an object role as a person reads it — "viewer", "editor". Not the
	// stored constant dressed up: templates interpolate it into a sentence, and
	// the label is what makes that sentence grammatical.
	Role string
	// The three fields an administrator's announcement carries. Named for the
	// one message that uses them rather than Subject/Body, because a field
	// called Body in a struct nine templates read is an invitation for the
	// tenth to interpolate administrator free text it never meant to.
	//
	// The body is plain text with its line breaks preserved by the frame, not
	// markup: a Markdown subset would need a parser, a sanitiser and a
	// disagreement with the composer about what it did.
	AnnouncementSubject string
	AnnouncementBody    string
	// LinkText is what the announcement's one link is called. The URL itself is
	// Link, above — validated at save and again immediately before the send,
	// because a javascript: URL inside mail wearing this install's From address
	// is a phishing primitive.
	LinkText string
	// Note is the sharer's own words, and it is optional.
	//
	// Its own field rather than Reason, which is a *denial's* words. They are
	// both free text somebody typed and both escaped the same way, and that is
	// the whole of what they have in common: a template that read the note out
	// of Reason would be one rename away from telling somebody their access was
	// declined for the reason "here you go, this is the payroll dashboard".
	Note string
	// ActivationMessage is the administrator's own words on letting somebody
	// in — the counterpart of the refusals shown at the door, and the only
	// sentence of this notification that is not the platform's.
	//
	// Its own field for the reason Note has one. It is install-wide standing
	// copy rather than something typed per person, and a template that read it
	// out of Reason or Note would be one rename away from congratulating
	// somebody with the sentence written for a refusal.
	ActivationMessage string
	// Address and PreviousAddress are an app's public URL after and before an
	// administrator switched its hostname on or off. Their own fields so the
	// one template that tells an owner their links moved cannot be fed
	// anything else.
	Address         string
	PreviousAddress string
}

// Origin is the line that says who sent a message and from where: "Sent by
// <sender> at <address>". The one formatter, so mail's footers and Slack's
// context line cannot word it two ways.
//
// The address always follows the name. It is the part no administrator can
// type, and it is how a recipient checks a name they do not recognise.
func Origin(sender, baseURL string) string {
	if baseURL == "" {
		return "Sent by " + sender + "."
	}
	return "Sent by " + sender + " at " + baseURL
}
