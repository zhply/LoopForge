package domain

import "testing"

func TestGongfengMRURLGitLabPrefix(t *testing.T) {
	cases := []struct {
		project string
		iid     int
		want    string
	}{
		{"gitlab:gitlab.example.com/group/app", 5, "https://gitlab.example.com/group/app/-/merge_requests/5"},
		{"gitlab:gitlab.corp/a/b/c", 1, "https://gitlab.corp/a/b/c/-/merge_requests/1"},
		{"github:octo/app", 2, "https://github.com/octo/app/pull/2"},
		{"group/app", 3, "https://git.example.com/group/app/-/merge_requests/3"},
	}
	for _, c := range cases {
		if got := GongfengMRURL(c.project, c.iid); got != c.want {
			t.Fatalf("GongfengMRURL(%q, %d) = %q; want %q", c.project, c.iid, got, c.want)
		}
	}
}

func TestSplitGitLabProject(t *testing.T) {
	cases := []struct {
		in   string
		host string
		path string
		ok   bool
	}{
		{"gitlab:gitlab.example.com/group/app", "gitlab.example.com", "group/app", true},
		{"gitlab:host/repo", "host", "repo", true},
		{"gongfeng:group/app", "", "", false},
		{"group/app", "", "", false},
		{"gitlab:", "", "", false},
		{"gitlab:hostonly", "", "", false},
		{"gitlab:host/", "", "", false},
	}
	for _, c := range cases {
		host, path, ok := SplitGitLabProject(c.in)
		if ok != c.ok || host != c.host || path != c.path {
			t.Fatalf("SplitGitLabProject(%q) = (%q, %q, %v); want (%q, %q, %v)", c.in, host, path, ok, c.host, c.path, c.ok)
		}
	}
}

func TestParseGongfengMRsGitLabURLs(t *testing.T) {
	text := "see https://gitlab.example.com/group/app/-/merge_requests/5 and https://gitlab.other.io/team/repo/-/merge_requests/12 and https://git.example.com/group/legacy/-/merge_requests/8 and https://github.com/octo/app/pull/4"
	refs := ParseGongfengMRs(text)
	if len(refs) != 4 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Project != "gitlab:gitlab.example.com/group/app" || refs[0].IID != 5 {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if refs[0].URL != "https://gitlab.example.com/group/app/-/merge_requests/5" {
		t.Fatalf("refs[0].URL = %q", refs[0].URL)
	}
	if refs[1].Project != "gitlab:gitlab.other.io/team/repo" || refs[1].IID != 12 {
		t.Fatalf("refs[1] = %+v", refs[1])
	}
	if refs[2].Project != "group/legacy" || refs[2].IID != 8 {
		t.Fatalf("refs[2] = %+v", refs[2])
	}
	if refs[3].Project != "github:octo/app" || refs[3].IID != 4 {
		t.Fatalf("refs[3] = %+v", refs[3])
	}
}

func TestParseGongfengMRsIgnoresGitLabHostedOnGongfengInstance(t *testing.T) {
	// git.example.com stays a Gongfeng project even with the /-/ form.
	refs := ParseGongfengMRs("https://git.example.com/group/app/-/merge_requests/8")
	if len(refs) != 1 || refs[0].Project != "group/app" || refs[0].IID != 8 {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestParseGongfengMRsLegacyFormsUnchanged(t *testing.T) {
	refs := ParseGongfengMRs("https://git.example.com/group/app/merge_requests/8")
	if len(refs) != 1 || refs[0].Project != "group/app" || refs[0].IID != 8 || refs[0].URL != "https://git.example.com/group/app/-/merge_requests/8" {
		t.Fatalf("refs = %+v", refs)
	}
	refs = ParseGongfengMRs("https://github.com/octo/app/pull/4")
	if len(refs) != 1 || refs[0].Project != "github:octo/app" || refs[0].IID != 4 {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestNormalizeScheduleSourceGitLabJira(t *testing.T) {
	for _, raw := range []string{"gitlab", "jira", "GitHub", "TAPD", "", "all"} {
		if _, err := NormalizeScheduleSource(raw); err != nil {
			t.Fatalf("NormalizeScheduleSource(%q) = %v", raw, err)
		}
	}
	if _, err := NormalizeScheduleSource("svn"); err == nil {
		t.Fatal("svn should be rejected")
	}
}
