package main

// `toft task` is what is left of scheduled tasks: a refusal that says where the
// work went. Tasks were removed in favour of jobs (ADR § One automation
// concept), and a script or an agent that still types the old command gets the
// replacement rather than "unknown command".
func cmdTask(_ *env, _ []string) error {
	return usageHinted("removed_command",
		"`toft job list` shows the jobs that replaced tasks; `toft job --help` lists the job commands",
		"toft task is gone: scheduled tasks were replaced by jobs. Declare [jobs.<name>] with a "+
			"command and a schedule in toft.toml, deploy, and see it with toft job list")
}
