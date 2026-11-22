package webfront

import "strings"

// Directive is one CSP directive: its lower-case name and its sources.
type Directive struct {
	Name    string
	Sources []string
}

// Policy is one serialized policy (one header value) as an ordered list.
type Policy []Directive

// ParsePolicy splits one policy. Several policies in one header value
// (comma-separated) are not merged here: use ParsePolicies.
func ParsePolicy(value string) Policy {
	var policy Policy
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		// The first occurrence wins, as in browsers.
		if seen[name] {
			continue
		}
		seen[name] = true
		policy = append(policy, Directive{Name: name, Sources: fields[1:]})
	}
	return policy
}

// ParsePolicies splits header values that may each hold several
// comma-separated policies.
func ParsePolicies(values []string) []Policy {
	var policies []Policy
	for _, value := range values {
		for _, one := range strings.Split(value, ",") {
			if policy := ParsePolicy(one); len(policy) != 0 {
				policies = append(policies, policy)
			}
		}
	}
	return policies
}

// Get returns a directive's sources and whether it is present.
func (p Policy) Get(name string) ([]string, bool) {
	for _, d := range p {
		if d.Name == name {
			return d.Sources, true
		}
	}
	return nil, false
}

// Set replaces a directive, or appends it when absent.
func (p Policy) Set(name string, sources ...string) Policy {
	for i, d := range p {
		if d.Name == name {
			out := append(Policy{}, p...)
			out[i] = Directive{Name: name, Sources: sources}
			return out
		}
	}
	return append(append(Policy{}, p...), Directive{Name: name, Sources: sources})
}

// Without drops directives by name.
func (p Policy) Without(names ...string) Policy {
	var out Policy
	for _, d := range p {
		drop := false
		for _, name := range names {
			if d.Name == name {
				drop = true
			}
		}
		if !drop {
			out = append(out, d)
		}
	}
	return out
}

// String serializes the policy.
func (p Policy) String() string {
	parts := make([]string, 0, len(p))
	for _, d := range p {
		parts = append(parts, strings.TrimSpace(d.Name+" "+strings.Join(d.Sources, " ")))
	}
	return strings.Join(parts, "; ")
}
