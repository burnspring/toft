package mail

import (
	_ "embed"
	"strings"
)

// The Toft mark, carried inside every message that draws it.
//
// Attached and referenced by Content-ID rather than linked: Gmail and Outlook
// hold a linked image behind "show images", Gmail strips a data: URI, and
// Gmail does not render SVG — so the one form every client shows on open is a
// PNG in a multipart/related part. It is web/public/apple-touch-icon.png, the
// same 180px mark a phone shows for the install, drawn at 24px.

//go:embed toft-mark.png
var markPNG []byte

// markName is the embedded part's file name, and so its Content-ID: go-mail
// writes `Content-ID: <toft-mark.png>` for an embed that names none.
const markName = "toft-mark.png"

// markImg is the mark as the frames draw it. Width and height are attributes
// as well as style, because Outlook reads only the attributes.
const markImg = `<img src="cid:` + markName + `" width="24" height="24" alt="" ` +
	`style="display:inline-block;width:24px;height:24px;border:0;border-radius:6px;vertical-align:middle;">`

// usesMark reports whether a message's HTML draws the mark, so the image rides
// only along with HTML that shows it — a plain-text message carries nothing.
func usesMark(html string) bool { return strings.Contains(html, "cid:"+markName) }
