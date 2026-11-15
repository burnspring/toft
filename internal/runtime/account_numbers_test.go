package runtime

import (
	"strings"
	"testing"
)

// shadowCalls swaps runShadow for a recorder, so the account commands a start
// would run can be read on a box where they cannot be.
func shadowCalls(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := runShadow
	runShadow = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { runShadow = orig })
	return &calls
}

// An auto-created per-app account is made at its recorded number — uid and
// per-app gid alike — never at the number useradd would pick, which was the
// next free one in the order apps happened to start after a restart (ADR §122).
func TestAnAppAccountIsCreatedAtItsRecordedNumber(t *testing.T) {
	calls := shadowCalls(t)
	asked := ""
	i := Isolation{PerApp: true, UserPrefix: "toft-app-", AutoCreate: true, Group: "toft-drills-nonexistent",
		Numbers: func(name string) (int, error) { asked = name; return 63007, nil }}

	// The recorder creates nothing, so the final lookup fails: what is under
	// test is the commands, and that the number came from Numbers.
	_, _ = i.resolve("reports")
	if asked != "toft-app-reports" {
		t.Errorf("Numbers asked for %q", asked)
	}
	got := strings.Join(*calls, "\n")
	for _, want := range []string{
		"groupadd --system --gid 63007 toft-app-reports",
		"useradd --system --uid 63007 --gid toft-drills-nonexistent --groups toft-app-reports",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

// A number this host already gives another name is refused rather than
// shared: starting the app as it would hand that account's files to the app.
func TestARecordedNumberHeldByAnotherNameIsRefused(t *testing.T) {
	calls := shadowCalls(t)
	i := Isolation{PerApp: true, UserPrefix: "toft-app-", AutoCreate: true,
		Numbers: func(string) (int, error) { return 0, nil }} // gid and uid 0: root's
	_, err := i.resolve("reports")
	if err == nil || !strings.Contains(err.Error(), "APP_ACCOUNT_ID_RANGE") {
		t.Fatalf("err = %v, want a refusal naming APP_ACCOUNT_ID_RANGE", err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "usermod") {
			t.Errorf("an account was written despite the refusal: %s", c)
		}
	}
}

// A deleted app's auto-created account leaves the host, and only an account
// Toft made: a pre-provisioned one, or the shared RunAsUser, is the operator's.
func TestRemoveAccountTouchesOnlyAutoCreatedOnes(t *testing.T) {
	calls := shadowCalls(t)
	if err := (Isolation{PerApp: true, UserPrefix: "toft-app-"}).RemoveAccount("reports"); err != nil || len(*calls) != 0 {
		t.Errorf("a pre-provisioned account was removed: %v %v", err, *calls)
	}
	if err := (Isolation{RunAsUser: "apps", AutoCreate: true}).RemoveAccount("reports"); err != nil || len(*calls) != 0 {
		t.Errorf("the shared account was removed: %v %v", err, *calls)
	}
	if got := (Isolation{PerApp: true, UserPrefix: "toft-app-"}).AccountName("reports"); got != "toft-app-reports" {
		t.Errorf("AccountName = %q", got)
	}
}

// The account range may be 32-bit — a number is never reused, so two thousand
// would not outlast a week of the e2e suite — but stays under the largest uid a
// plain tar header holds, and never contains nobody's 65534 or the 16-bit -1.
func TestTheAccountRangeIsWideAndAvoidsTheSpecialIDs(t *testing.T) {
	for spec, ok := range map[string]bool{
		"":               true,
		"100000-1999999": true,
		"63000-64999":    true,
		"1000-65533":     true,
		"999-5000":       false,
		"60000-70000":    false, // contains 65534 and 65535
		"100000-2097152": false,
		"200000-100000":  false,
		"100000":         false,
	} {
		if _, _, err := ParseAccountIDRange(spec); (err == nil) != ok {
			t.Errorf("ParseAccountIDRange(%q) err = %v, want ok=%v", spec, err, ok)
		}
	}
}
