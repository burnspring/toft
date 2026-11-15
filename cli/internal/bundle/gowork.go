package bundle

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// workspaceDirsLeftOut is each directory a root go.work `use`s that exists on
// this machine and contributed nothing to the bundle — ignored, usually, as
// `toft sdk vendor` leaves .toft/sdk-go in .gitignore. The build then fails
// "cannot load module … listed in go.work" (BB-028), and a check that read the
// bundle passed; this is what lets the client say so before the deploy.
func workspaceDirsLeftOut(root string, files []File) []string {
	raw, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return nil
	}
	shipped := false
	for _, f := range files {
		if f.Rel == "go.work" {
			shipped = true
			break
		}
	}
	if !shipped {
		return nil
	}
	var out []string
	for _, dir := range goWorkUses(string(raw)) {
		clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(dir), "./"))
		if clean == "." || clean == "" || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(clean))); err != nil || !st.IsDir() {
			continue
		}
		found := false
		for _, f := range files {
			if strings.HasPrefix(f.Rel, clean+"/") {
				found = true
				break
			}
		}
		if !found {
			out = append(out, clean)
		}
	}
	return out
}

// goWorkUses is every directory a go.work's `use` directives name, in both the
// one-line and the block form. Comments are dropped; quoting is undone.
func goWorkUses(text string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			out = append(out, unquote(line))
			continue
		}
		rest, ok := strings.CutPrefix(line, "use")
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '(') {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "(" {
			inBlock = true
			continue
		}
		if rest != "" {
			out = append(out, unquote(rest))
		}
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '`' && s[len(s)-1] == '`') {
		return s[1 : len(s)-1]
	}
	return s
}
