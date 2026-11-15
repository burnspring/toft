// Package skills carries the two agent skills `toft` ships, so the binary can
// print the ones written for it.
//
// **The skills and the binary are published side by side and installed
// apart.** An agent holds whichever SKILL.md somebody copied into its skills
// directory and whichever `toft` is on PATH, and nothing makes those two the
// same release: a skill describing flags the binary does not have — or
// missing the ones it does — is followed faithfully and reported as a product
// defect. Embedding them makes `toft skill` the copy that cannot disagree with
// the binary it came out of.
//
// This file is the one Go file under skills/, and scripts/publish-cli.sh
// leaves it out of the public download repository, which carries no source.
package skills

import "embed"

//go:embed toft-cli/SKILL.md toft-sdk/SKILL.md
var files embed.FS

// Names are the skills, in the order `toft skill` lists them.
var Names = []string{"toft-cli", "toft-sdk"}

// Get returns one skill's SKILL.md, or false for a name that is not one.
func Get(name string) ([]byte, bool) {
	b, err := files.ReadFile(name + "/SKILL.md")
	return b, err == nil
}
