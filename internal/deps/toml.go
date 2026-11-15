package deps

import "github.com/BurntSushi/toml"

// decodeTOML is a one-line wrapper so that resolve.go reads the same way for
// every ecosystem's lockfile, and so the TOML dependency has exactly one
// import site in this package.
func decodeTOML(path string, into any) error {
	_, err := toml.DecodeFile(path, into)
	return err
}
