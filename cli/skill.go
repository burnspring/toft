package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toft/deploy/cli/skills"
)

// `toft skill` — the agent skills this binary was built with.
//
// The skills are published beside the binary and installed apart from it, so
// the copy in an agent's skills directory can be a release older or newer than
// the `toft` on PATH. These are the bytes from this binary's own tree, which is
// the one copy that describes exactly the commands it has. See
// deploy/cli/skills/skills.go.
//
// **What `toft skill install` writes is a pointer, not a copy.** The installed
// SKILL.md keeps the real name and description, which is all an agent reads to
// decide to load it, and its body says to run `toft skill <name>`. So the text
// an agent follows always comes out of the binary on PATH, and upgrading `toft`
// is the whole of updating its skills: a copied file was the one thing that
// went stale with nothing to notice.

const skillUsage = `toft skill [name] — print an agent skill written for this binary

Usage:
  toft skill                 List them
  toft skill toft-cli        Deploying with toft
  toft skill toft-sdk        Writing an app against the platform's SDK
  toft skill install         Install both into your coding agents' skills folders

install writes a short SKILL.md for each that tells the agent to run
toft skill <name>, so the instructions always match this binary and never
need updating. It writes into ~/.claude/skills and ~/.agents/skills, for
whichever of ~/.claude and ~/.agents exists, and is run by install.sh.

Flags:
  --dir DIR           install: write into this skills folder instead,
                      e.g. .claude/skills for one project
  --json              The names, {name, content}, or the paths written
`

func cmdSkill(e *env, args []string) error {
	var g globalFlags
	var dir string
	fs := newFlags("skill", skillUsage)
	g.register(fs)
	fs.StringVar(&dir, "dir", "", "install: the skills folder to write into")
	name, rest := takeOperand(args)
	if err := parseAfter(fs, rest, name); err != nil {
		return skipHelp(err)
	}
	name = operandOr(name, fs)
	// No session: a skill is a property of this binary, and printing one
	// needs no install and no credential.
	e.buildOut(&g)
	e.json = g.json

	if name == "" {
		if g.json {
			return writeNDJSON(e.out.Stdout, skills.Names)
		}
		for _, n := range skills.Names {
			e.out.Answer("%s", n)
		}
		if e.out.Rich() {
			e.out.Next("toft skill <name>")
		} else {
			e.out.Dim("toft skill <name> prints one.")
		}
		return nil
	}
	if name == "install" {
		return installSkills(e, dir)
	}
	if dir != "" {
		return usageCodef("conflicting_flags", "--dir goes with toft skill install")
	}
	body, ok := skills.Get(name)
	if !ok {
		return usageCodef("unknown_skill", "no skill called %q — there are %s", name, strings.Join(skills.Names, " and "))
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]string{"name": name, "content": string(body)})
	}
	fmt.Fprint(e.out.Stdout, string(body))
	return nil
}

// agentSkillRoots are the agent folders `toft skill install` writes into by
// default, each with the skills folder under it. Only a folder that exists is
// used: the installer runs this on machines with no coding agent at all, and
// creating ~/.agents there is clutter nobody asked for.
var agentSkillRoots = []string{".claude", ".agents"}

func installSkills(e *env, dir string) error {
	var targets []string
	if dir != "" {
		targets = []string{dir}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return usageCodef("no_home", "no home directory to install into; name one with --dir")
		}
		for _, r := range agentSkillRoots {
			if st, err := os.Stat(filepath.Join(home, r)); err == nil && st.IsDir() {
				targets = append(targets, filepath.Join(home, r, "skills"))
			}
		}
	}
	// Nothing to write into is a refusal, not a success that did nothing
	// (BB-022): the installer ignores the exit, and a person or agent who ran
	// it learns that no skill landed.
	if len(targets) == 0 {
		return usageHinted("no_agent_folder", "name the folder to write into with --dir, like `toft skill install --dir ~/.claude/skills`",
			"no coding agent folder found (~/.claude or ~/.agents) — nothing was installed")
	}
	var written []string
	for _, t := range targets {
		for _, n := range skills.Names {
			body, _ := skills.Get(n)
			p := filepath.Join(t, n, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, skillPointer(n, body), 0o644); err != nil {
				return err
			}
			written = append(written, p)
		}
	}
	if e.json {
		return writeNDJSON(e.out.Stdout, written)
	}
	for _, p := range written {
		e.out.Answer("%s", p)
	}
	return nil
}

// skillPointer is the SKILL.md `toft skill install` writes: the embedded
// skill's own front matter, and a body that sends the agent to the binary.
func skillPointer(name string, body []byte) []byte {
	const tick = "`"
	return []byte(frontMatter(string(body)) + "\n" +
		"# " + name + "\n\n" +
		"This file is a pointer, written by " + tick + "toft skill install" + tick + ". The\n" +
		"instructions are printed by the " + tick + "toft" + tick + " binary on this machine, so they\n" +
		"always match the commands it has.\n\n" +
		"**Run this and follow what it prints:**\n\n" +
		"```bash\ntoft skill " + name + "\n```\n\n" +
		"If " + tick + "toft" + tick + " is not installed, install it and run the command above:\n\n" +
		"```bash\n" + upgradeCommand + "\n```\n")
}

// frontMatter returns the leading `---` block of a SKILL.md, delimiters
// included, or "" when there is none.
func frontMatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return ""
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return ""
	}
	return s[:4+end+5]
}
