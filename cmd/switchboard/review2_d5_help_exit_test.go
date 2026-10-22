package main

// Bug: CHANGELOG.md (v1.0.0, "From the release review") says "`-h` exits 0 in every command (it was a usage error's
// 2)", and main.go's package comment says "A usage error exits 2 (-h, help asked for, exits 0)". But the fix
// (helpOrUsage) only covers serve, check and usage, whose flags go through a FlagSet: `switchboard -h`, the first
// thing a new user types, and `switchboard --help`, `switchboard help`, `switchboard version -h` and
// `switchboard keygen -h` still print the usage line and exit 2. v1.0.0 promises the exit codes (CHANGELOG "What a
// 1.x release keeps"), so this is either fixed before the tag or the CHANGELOG narrowed to "serve, check and usage".

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestReviewD5HelpExitsZeroInEveryCommand(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"help"}, {"version", "-h"}, {"keygen", "-h"},
		{"keygen", "--help"}} {
		var errs bytes.Buffer
		if code := run(context.Background(), args, noEnv, io.Discard, &errs, nil); code != 0 ||
			!strings.Contains(errs.String(), "usage") {
			t.Errorf("switchboard %s: exit %d (help asked for, so 0): %s", strings.Join(args, " "), code, errs.String())
		}
	}
}

// And only help: an unknown word or flag is still a usage error (2).
func TestReviewD5OtherWordsAreStillUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"-x"}, {"helpme"}, {"version", "-x"}, {"keygen", "help", "me"}} {
		if code := run(context.Background(), args, noEnv, io.Discard, io.Discard, nil); code != 2 {
			t.Errorf("switchboard %s: exit %d, want 2", strings.Join(args, " "), code)
		}
	}
}
