package db

import "testing"

func TestProfileUserAgentRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	p := &LLMProfile{Name: "ua-roundtrip", Format: "openai", Model: "test", APIKey: "key-original"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	p, err = d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("load: %v", err)
	}
	if p.UserAgent != "" {
		t.Fatalf("default UA=%q", p.UserAgent)
	}
	for _, step := range []struct{ ua, key, wantKey string }{
		{"agent/1.0", "", "key-original"},
		{"agent/2.0", "key-replaced", "key-replaced"},
		{"", "", "key-replaced"},
	} {
		p.UserAgent, p.APIKey = step.ua, step.key
		if _, err := d.SaveProfile(p); err != nil {
			t.Fatal(err)
		}
		got, err := d.ProfileByID(id)
		if err != nil || got == nil {
			t.Fatalf("load: %v", err)
		}
		if got.UserAgent != step.ua || got.APIKey != step.wantKey {
			t.Fatalf("UA=%q key preserved=%v", got.UserAgent, got.APIKey == step.wantKey)
		}
		for _, load := range []func() ([]*LLMProfile, error){d.ListProfiles, d.PoolProfiles} {
			rows, err := load()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.ID == id {
					found = true
					if row.UserAgent != step.ua {
						t.Fatalf("list UA=%q want %q", row.UserAgent, step.ua)
					}
				}
			}
			if !found {
				t.Fatal("profile missing from list")
			}
		}
	}
}
