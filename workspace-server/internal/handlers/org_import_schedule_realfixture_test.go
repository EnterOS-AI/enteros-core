package handlers

// The REAL schedules declared across every Molecule template repo, captured
// 2026-07-27 from the live trees (16 cloned + 4 private read through the Gitea
// contents API).
//
// This is the evidence behind M2's done condition. Before slugify-on-write, 35
// of these 37 were SKIPPED by renderTemplateSchedulesYAML for one reason —
// their authored name is a human display string, not a grid key — and so had
// never fired in any workspace. Two (platform-agent's) were already kebab and
// rendered fine; they must keep rendering byte-identically.
//
// Path matters: renderTemplateSchedulesYAML runs PER WORKSPACE, so the grid
// namespace is the declaring file, not the repo. molecule-dev declares an
// "Orchestrator pulse" in three separate team files; those are three different
// workspaces and do not collide with each other.

type realTemplateSchedule struct {
	Repo string
	Path string
	Name string
	Cron string
	TZ   string
}

var realTemplateSchedules = []realTemplateSchedule{
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "technical-researcher/workspace.yaml", Name: "Hourly plugin curation", Cron: "22 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "content-marketer/workspace.yaml", Name: "Hourly topic queue refresh", Cron: "41 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "teams/pm.yaml", Name: "Orchestrator pulse", Cron: "1,6,11,16,21,26,31,36,41,46,51,56 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "teams/marketing.yaml", Name: "Orchestrator pulse (every 5 min)", Cron: "4,9,14,19,24,29,34,39,44,49,54,59 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "teams/research.yaml", Name: "Orchestrator pulse (every 5 min)", Cron: "4,9,14,19,24,29,34,39,44,49,54,59 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "seo-growth-analyst/workspace.yaml", Name: "Daily Lighthouse + keyword audit", Cron: "23 8 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "product-marketing-manager/workspace.yaml", Name: "Hourly competitor diff", Cron: "33 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "social-media-brand/workspace.yaml", Name: "Hourly mention monitor", Cron: "27 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-dev", Path: "community-manager/workspace.yaml", Name: "Hourly unanswered sweep", Cron: "12 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-worker-gemini", Path: "org.yaml", Name: "Security audit (every 12h)", Cron: "0 */12 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-worker-gemini", Path: "org.yaml", Name: "Code quality audit (every 12h)", Cron: "0 6,18 * * *", TZ: ""},
	{Repo: "molecule-ai-workspace-template-platform-agent", Path: "config.yaml", Name: "daily-activity-report", Cron: "0 9 * * *", TZ: "UTC"},
	{Repo: "molecule-ai-workspace-template-platform-agent", Path: "config.yaml", Name: "plugin-auto-update", Cron: "0 3 * * *", TZ: "UTC"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "A) Continuous agent tick", Cron: "*/30 * * * *", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Mon defensive: GSC week-over-week", Cron: "0 8 * * 1", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Tue defensive: hreflang", Cron: "0 6 * * 2", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Wed defensive: technical", Cron: "0 6 * * 3", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Thu defensive: maps + GBP", Cron: "0 6 * * 4", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Fri defensive: backlinks", Cron: "0 6 * * 5", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Sat defensive: content E-E-A-T", Cron: "0 8 * * 6", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Sat offensive: keyword cluster", Cron: "0 10 * * 6", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Twice-monthly audit (1st & 15th)", Cron: "0 9 1,15 * *", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Monthly: competitor pages", Cron: "0 11 1 * *", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Monthly: strategy plan refresh", Cron: "0 9 2 * *", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-workspace-template-seo-agent", Path: "config.yaml", Name: "B) Monthly: GEO / AI Overview", Cron: "0 9 3 * *", TZ: "America/Vancouver"},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Heartbeat (every 30m)", Cron: "*/30 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Memory Compactor (every 6h)", Cron: "0 */6 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Daily Summary (9 PM Vancouver)", Cron: "0 21 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Health Check (every 1h)", Cron: "0 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "SEO Builder (daily 6:17 AM)", Cron: "17 6 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "SEO Weekly Report (Monday 8:03 AM)", Cron: "3 8 * * 1", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Social Media Poster (every 6h)", Cron: "0 */6 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Social Media Monitor (every 30m)", Cron: "*/30 * * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Social Media Engage (every 6h)", Cron: "30 */6 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Citation Builder (daily 7:30 AM)", Cron: "30 7 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-reno-stars", Path: "org.yaml", Name: "Email Classification Review (daily 9 AM)", Cron: "0 9 * * *", TZ: ""},
	{Repo: "molecule-ai-org-template-molecule-production", Path: "teams/pm.yaml", Name: "Orchestrator pulse", Cron: "1,6,11,16,21,26,31,36,41,46,51,56 * * * *", TZ: ""},
}
