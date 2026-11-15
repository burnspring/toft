package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
)

// The two halves of `toft fetch` that move a file rather than a page: --form
// sends one up as multipart, --output brings a body down to disk. Both exist
// because the way round them was --json and a script to pull the body back
// out of a JSON string, which corrupts anything that is not text.

// formList collects repeated --form k=v and k=@file, in order.
type formList []string

func (l *formList) String() string { return "" }

func (l *formList) Set(v string) error {
	k, _, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--form takes name=value or name=@file, not %q", v)
	}
	*l = append(*l, v)
	return nil
}

// encode is the multipart body and its Content-Type, boundary included.
func (l formList) encode() (string, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, field := range l {
		k, v, _ := strings.Cut(field, "=")
		if !strings.HasPrefix(v, "@") {
			if err := w.WriteField(k, v); err != nil {
				return "", nil, err
			}
			continue
		}
		path := v[1:]
		f, err := os.Open(path)
		if err != nil {
			return "", nil, err
		}
		part, err := w.CreateFormFile(k, filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		f.Close()
		if err != nil {
			return "", nil, err
		}
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}

// outputFile is --output: a temp file beside the target, renamed onto it only
// when the answer is the one asked for, so a refusal never overwrites a good
// file with an error page.
type outputFile struct {
	f      *os.File
	target string
	kept   bool
	stdout io.Writer // for "-"
}

// newOutputFile refuses before anything is asked of the app (BB-036, fourth
// 2026-11-03 list): a directory is not a file to write, and an existing file
// is replaced only with --force — store get's rule, the same code. "-" is
// stdout: the body is held here until the answer is known, then copied out,
// so a refusal's page never lands where the file was wanted.
func newOutputFile(target string, force bool, stdout io.Writer) (*outputFile, error) {
	if target == "-" {
		f, err := os.CreateTemp("", "toft-fetch-*")
		if err != nil {
			return nil, usageCodef("unwritable_file", "--output -: cannot hold the body: %v", err)
		}
		return &outputFile{f: f, target: target, stdout: stdout}, nil
	}
	if st, err := os.Stat(target); err == nil {
		if st.IsDir() {
			return nil, usageHinted("is_a_directory", "name a file: -o "+filepath.Join(target, "<name>"),
				"--output %s is a directory, not a file to write", target)
		}
		if !force {
			return nil, usageCodef("file_exists", "%s already exists — pass --force to replace it", target)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return nil, usageCodef("unwritable_file", "--output %s: cannot write in %s: %v",
			target, filepath.Dir(target), errors.Unwrap(err))
	}
	return &outputFile{f: f, target: target}, nil
}

// keep closes the temp file and gives it the target's name — or, for "-",
// copies it to stdout.
func (o *outputFile) keep() error {
	if o.target == "-" {
		if _, err := o.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(o.stdout, o.f); err != nil {
			return err
		}
		o.f.Close()
		os.Remove(o.f.Name())
		o.kept = true
		return nil
	}
	if err := o.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(o.f.Name(), o.target); err != nil {
		os.Remove(o.f.Name())
		o.kept = true // nothing left to discard
		return fmt.Errorf("cannot write %s: %v", o.target, errors.Unwrap(err))
	}
	o.kept = true
	return nil
}

// head is the first max bytes written, for printing a refusal; max 0 is all.
func (o *outputFile) head(max int64) (string, bool) {
	if _, err := o.f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	var r io.Reader = o.f
	if max > 0 {
		r = io.LimitReader(o.f, max+1)
	}
	raw, _ := io.ReadAll(r)
	if max > 0 && int64(len(raw)) > max {
		return string(raw[:max]), true
	}
	return string(raw), false
}

// discard removes the temp file unless keep renamed it.
func (o *outputFile) discard() {
	if o.kept {
		return
	}
	o.f.Close()
	os.Remove(o.f.Name())
}
