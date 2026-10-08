package connect

import "testing"

func TestResolvePrecedence(t *testing.T) {
	rules := []Rule{
		{MatchHost: "db-p-01", User: "root"},
		{MatchTag: "prod", User: "deploy"},
		{MatchTag: "dev", User: "deploy"},
	}

	cases := []struct {
		name     string
		target   Target
		override string
		want     string
		wantErr  bool
	}{
		{
			name:   "host rule wins over tag rule",
			target: Target{Hostname: "db-p-01", Tags: []string{"prod"}},
			want:   "root",
		},
		{
			name:   "tag rule applies when no host rule matches",
			target: Target{Hostname: "kolibri-prod-app-1", Tags: []string{"prod", "web"}},
			want:   "deploy",
		},
		{
			name:     "CLI override wins over everything, even a host rule",
			target:   Target{Hostname: "db-p-01", Tags: []string{"prod"}},
			override: "stian",
			want:     "stian",
		},
		{
			name:    "no match and no override is an error, not a silent default",
			target:  Target{Hostname: "mystery-box", Tags: []string{"untagged"}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(rules, c.target, c.override)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got user %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestTargetHasTag(t *testing.T) {
	target := Target{Hostname: "x", Tags: []string{"prod", "web"}}
	if !target.HasTag("web") {
		t.Fatal("expected HasTag(\"web\") to be true")
	}
	if target.HasTag("dev") {
		t.Fatal("expected HasTag(\"dev\") to be false")
	}
}
