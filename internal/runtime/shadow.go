package runtime

import "sync"

// The shadow suite — groupadd, useradd, usermod, gpasswd — is the one place the
// runtime writes the host's account database, and every call to it goes
// through here.
//
// **Its lock file is not a queue.** Each tool takes /etc/.pwd.lock (or
// /etc/group.lock) and a second writer that finds it held fails with "cannot
// lock /etc/group; try again later" rather than waiting. Starts do run at once:
// Supervisor.Remount brings four apps up together, boot Reconcile starts every
// app, and a host job attaches storage independently of its app. Without this
// lock a start that would have succeeded a second later fails, and the app is
// `crashed` for a reason nobody can reproduce.
//
// The lock is also what makes a check-then-create atomic. LookupGroup and then
// groupadd is two steps, and two starts asking about one group would otherwise
// both see it missing and the second groupadd would fail on a group that now
// exists. So every caller holds the lock across its look and its write
// (lockShadow), and the write itself is runShadow.
var shadowMu sync.Mutex

// lockShadow takes the account-database lock and returns its release, for a
// caller that must look and then write without another start in between:
//
//	defer lockShadow()()
func lockShadow() func() {
	shadowMu.Lock()
	return shadowMu.Unlock
}

// runShadow runs one shadow-suite command. It does not take the lock: every
// caller already holds it, because every caller looked first. Injectable so a
// test can watch for two of them overlapping on a box where it cannot run
// them.
var runShadow = func(name string, args ...string) ([]byte, error) {
	return command(name, args...).CombinedOutput()
}
