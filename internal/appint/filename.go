package appint

import (
	"fmt"
	"strings"
)

// The one rule about a name an app supplies for a file it is placing somewhere.
//
// **The folder is the attachment's and the name is the app's**, and a name that
// tries to be a path is refused rather than sanitised. A sanitiser that quietly
// rewrites `reports/june.csv` to `june.csv` hands an app a file it cannot find
// again, in a folder it cannot list, under a name it did not choose — and the
// app has no way to learn that happened. A 400 naming the character is the
// version of this an author can act on.
//
// It is deliberately not the same function as safeFilename, which reduces an
// uploaded *part's* filename to a basename. That one is defending against a
// header somebody else wrote; this one is answering a field an app filled in on
// purpose, and the right answer to a deliberate path is "no", not "here is
// something else".

// reservedDeviceNames are the DOS device names Windows still refuses, with or
// without an extension. A file called CON in a SharePoint library is a file
// nobody on Windows can open, download or delete.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// CheckFilename refuses a name that is not just a name.
func CheckFilename(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return fmt.Errorf("a file needs a name")
	case strings.Contains(trimmed, "/"):
		return fmt.Errorf("%q contains a %q — an app names its file and never where it goes; "+
			"the folder is the one an administrator chose", name, "/")
	case strings.Contains(trimmed, `\`):
		return fmt.Errorf(`%q contains a "\" — an app names its file and never where it goes; `+
			"the folder is the one an administrator chose", name)
	case strings.HasPrefix(trimmed, "."):
		return fmt.Errorf("%q starts with a dot — that is a hidden file in some systems and a "+
			"relative path in others, so it is refused rather than guessed at", name)
	}
	base := trimmed
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if reservedDeviceNames[strings.ToUpper(base)] {
		return fmt.Errorf("%q is a reserved device name — a file called that is one nobody on "+
			"Windows can open, download or delete", name)
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%q contains a control character", name)
		}
	}
	return nil
}
