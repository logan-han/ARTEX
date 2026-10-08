package db

import "testing"

func rule(kind, pattern string, enabled bool) AssetInterceptRule {
	return AssetInterceptRule{Kind: kind, Pattern: pattern, Enabled: enabled}
}

func TestMatchAssetInterceptRules(t *testing.T) {
	cases := []struct {
		name    string
		rules   []AssetInterceptRule
		domains []string
		ips     []string
		urls    []string
		want    bool
		wantVal string
	}{
		{"built-in fuzzy government domain matches", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
			[]string{"www.beijing.gov.cn"}, nil, nil, true, "www.beijing.gov.cn"},
		{"fuzzy education domain matches", []AssetInterceptRule{rule("fuzzy_domain", ".edu", true)},
			[]string{"mit.edu"}, nil, nil, true, "mit.edu"},
		{"exact domain matches case-insensitively", []AssetInterceptRule{rule("exact_domain", "Example.com", true)},
			[]string{"example.com"}, nil, nil, true, "example.com"},
		{"exact domain does not match a subdomain", []AssetInterceptRule{rule("exact_domain", "example.com", true)},
			[]string{"a.example.com"}, nil, nil, false, ""},
		{"exact IP matches", []AssetInterceptRule{rule("exact_ip", "203.0.113.5", true)},
			nil, []string{"203.0.113.5"}, nil, true, "203.0.113.5"},
		{"fuzzy IP prefix matches", []AssetInterceptRule{rule("fuzzy_ip", "203.0.113.", true)},
			nil, []string{"203.0.113.99"}, nil, true, "203.0.113.99"},
		{"CIDR matches", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"192.168.5.20"}, nil, true, "192.168.5.20"},
		{"CIDR does not match", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"10.0.0.1"}, nil, false, ""},
		{"exact URL matches", []AssetInterceptRule{rule("exact_url", "https://a.gov.cn/login", true)},
			nil, nil, []string{"https://a.gov.cn/login"}, true, "https://a.gov.cn/login"},
		{"fuzzy URL matches a path", []AssetInterceptRule{rule("fuzzy_url", "/admin", true)},
			nil, nil, []string{"https://x.com/admin/panel"}, true, "https://x.com/admin/panel"},
		{"disabled rule does not match", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", false)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
		{"no rules means no match", nil, []string{"www.gov.cn"}, nil, nil, false, ""},
		{"empty pattern does not match", []AssetInterceptRule{rule("fuzzy_domain", "  ", true)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, val, ok := MatchAssetInterceptRules(c.rules, c.domains, c.ips, c.urls)
			if ok != c.want {
				t.Fatalf("matched = %v, want %v (rule=%+v)", ok, c.want, r)
			}
			if ok && val != c.wantVal {
				t.Fatalf("matched value = %q, want %q", val, c.wantVal)
			}
		})
	}
}

func TestEvaluateAssetGate(t *testing.T) {
	block := []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}
	allow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", true)}

	// 1. Matches a block rule → denied (the block reason takes precedence).
	if d := EvaluateAssetGate(block, allow, []string{"www.gov.cn"}, nil, nil); d.Allowed {
		t.Fatal("matching a block rule should be denied")
	}

	// 2. No block match, allow rules exist but none match → denied (not allowed).
	d := EvaluateAssetGate(block, allow, []string{"foo.other.com"}, nil, nil)
	if d.Allowed {
		t.Fatal("with an allowlist and no match it should be denied")
	}
	if d.Reason == "" {
		t.Fatal("a denial should carry a reason")
	}

	// 3. No block match, an allow rule matches → allowed.
	if d := EvaluateAssetGate(block, allow, []string{"api.example.com"}, nil, nil); !d.Allowed {
		t.Fatal("matching the allowlist should be allowed")
	}

	// 4. No allow rules (allowlist not enabled) → allowed as long as no block rule matches.
	if d := EvaluateAssetGate(block, nil, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("without an allowlist, no block match should be allowed")
	}

	// 5. All allow rules disabled → treated as no allowlist enabled, allowed.
	disabledAllow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", false)}
	if d := EvaluateAssetGate(nil, disabledAllow, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("should be allowed when the whole allowlist is disabled")
	}

	// 6. Block takes precedence over allow: a target matching both block and allow → denied.
	if d := EvaluateAssetGate(
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]string{"www.gov.cn"}, nil, nil,
	); d.Allowed {
		t.Fatal("block should take precedence over allow")
	}
}

func TestAssetInterceptCandidates(t *testing.T) {
	// A service asset with only a URL: the host should be split out into the domain candidates, so fuzzy_domain matches it.
	a := &Asset{Type: "service", URL: "https://portal.beijing.gov.cn:8443/app"}
	domains, _, urls := a.interceptCandidates()
	if len(urls) != 1 || urls[0] != a.URL {
		t.Fatalf("urls = %v", urls)
	}
	found := false
	for _, d := range domains {
		if d == "portal.beijing.gov.cn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("URL host not split into the domain candidates: %v", domains)
	}
	r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}, domains, nil, urls)
	if !ok {
		t.Fatalf("a government service asset with only a URL should be matched by fuzzy_domain, rule=%+v", r)
	}

	// When the URL host is an IP it should go into the IP candidates, so CIDR can match it.
	b := &Asset{Type: "service", URL: "http://10.1.2.3/x"}
	_, ips, _ := b.interceptCandidates()
	if r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("cidr", "10.0.0.0/8", true)}, nil, ips, nil); !ok {
		t.Fatalf("an IP in the URL should be matched by CIDR, ips=%v rule=%+v", ips, r)
	}
}
