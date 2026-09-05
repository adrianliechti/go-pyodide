package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Environment markers of the embedded interpreter, for PEP 508 evaluation.
var markerEnv = map[string]string{
	"python_version":                 "3.14",
	"python_full_version":            "3.14.7",
	"implementation_name":            "cpython",
	"platform_python_implementation": "CPython",
	"sys_platform":                   "wasi",
	"platform_system":                "WASI",
	"platform_machine":               "wasm32",
	"os_name":                        "posix",
	"implementation_version":         "3.14.7",
}

// Package is one resolved distribution.
type Package struct {
	Name    string // normalized
	Version string
	Wheel   string // file name of the pure wheel, empty if none
	URL     string
	Deps    []string // normalized names, markers already evaluated
	Pinned  bool
	Missing bool // no pure wheel: native extension or sdist only

	requires []string // raw Requires-Dist
}

// Resolver walks PyPI metadata, keeping only pure-Python wheels.
type Resolver struct {
	Packages map[string]*Package
	client   *http.Client
	extras   map[string]map[string]bool
}

func NewResolver() *Resolver {
	return &Resolver{
		Packages: map[string]*Package{},
		client:   http.DefaultClient,
		extras:   map[string]map[string]bool{},
	}
}

// Resolve adds a requirement such as "openpyxl" or "pdfminer.six==20251230"
// and everything it needs.
func (r *Resolver) Resolve(ctx context.Context, spec string) error {
	req, ok := parseRequirement(spec)
	if !ok {
		return fmt.Errorf("cannot parse requirement %q", spec)
	}
	return r.resolve(ctx, req)
}

func (r *Resolver) resolve(ctx context.Context, req requirement) error {
	seen := r.extras[req.name]
	if seen == nil {
		seen = map[string]bool{}
		r.extras[req.name] = seen
	}
	newExtras := false
	for _, e := range req.extras {
		if !seen[e] {
			seen[e] = true
			newExtras = true
		}
	}

	pkg, visited := r.Packages[req.name]
	if visited && !newExtras {
		return nil
	}
	if !visited {
		var err error
		pkg, err = r.fetch(ctx, req.name, req.pin)
		if err != nil {
			return err
		}
		r.Packages[req.name] = pkg
		if pkg.Missing {
			return nil
		}
	}

	for _, dep := range pkg.requires {
		d, ok := parseRequirement(dep)
		if !ok || !d.applies(seen) {
			continue
		}
		if !contains(pkg.Deps, d.name) {
			pkg.Deps = append(pkg.Deps, d.name)
		}
		if err := r.resolve(ctx, d); err != nil {
			return err
		}
	}
	sort.Strings(pkg.Deps)
	return nil
}

type pypiResponse struct {
	Info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		RequiresDist []string `json:"requires_dist"`
	} `json:"info"`
	URLs []struct {
		Filename    string `json:"filename"`
		URL         string `json:"url"`
		PackageType string `json:"packagetype"`
		Yanked      bool   `json:"yanked"`
	} `json:"urls"`
}

func (r *Resolver) fetch(ctx context.Context, name, version string) (*Package, error) {
	url := "https://pypi.org/pypi/" + name + "/json"
	if version != "" {
		url = "https://pypi.org/pypi/" + name + "/" + version + "/json"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var data pypiResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}

	pkg := &Package{Name: name, Version: data.Info.Version, Pinned: version != "", requires: data.Info.RequiresDist}
	for _, f := range data.URLs {
		if f.PackageType == "bdist_wheel" && strings.HasSuffix(f.Filename, "-none-any.whl") && !f.Yanked {
			pkg.Wheel, pkg.URL = f.Filename, f.URL
			break
		}
	}
	pkg.Missing = pkg.Wheel == ""
	return pkg, nil
}

// NeedsNative reports the packages in pkg's dependency closure that have no
// pure wheel.
func (r *Resolver) NeedsNative(name string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		p := r.Packages[n]
		if p == nil {
			return
		}
		if p.Missing {
			out = append(out, n)
			return
		}
		for _, d := range p.Deps {
			walk(d)
		}
	}
	for _, d := range r.Packages[name].Deps {
		walk(d)
	}
	sort.Strings(out)
	return out
}

// ---- PEP 508 requirements and markers ----

type requirement struct {
	name   string
	extras []string
	pin    string // exact version from "==", if any
	marker string
}

var nonNormal = regexp.MustCompile(`[-_.]+`)

func normalize(name string) string {
	return strings.ToLower(nonNormal.ReplaceAllString(strings.TrimSpace(name), "-"))
}

func parseRequirement(s string) (requirement, bool) {
	var req requirement
	if i := strings.Index(s, ";"); i >= 0 {
		req.marker = strings.TrimSpace(s[i+1:])
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if strings.Contains(s, "@") {
		return req, false // URL requirement
	}
	i := strings.IndexAny(s, "[(<>=!~ ")
	if i < 0 {
		i = len(s)
	}
	req.name = normalize(s[:i])
	if req.name == "" {
		return req, false
	}
	rest := s[i:]
	if j := strings.Index(rest, "["); j >= 0 {
		if k := strings.Index(rest, "]"); k > j {
			for _, e := range strings.Split(rest[j+1:k], ",") {
				req.extras = append(req.extras, normalize(e))
			}
			rest = rest[k+1:]
		}
	}
	rest = strings.Trim(strings.TrimSpace(rest), "()")
	for _, clause := range strings.Split(rest, ",") {
		clause = strings.TrimSpace(clause)
		if v, ok := strings.CutPrefix(clause, "=="); ok && !strings.HasPrefix(v, "=") && !strings.Contains(v, "*") {
			req.pin = strings.TrimSpace(v)
		}
	}
	return req, true
}

// applies evaluates the marker for the interpreter environment and the
// extras requested from the depending package.
func (req requirement) applies(extras map[string]bool) bool {
	if req.marker == "" {
		return true
	}
	if len(extras) == 0 {
		return evalMarker(req.marker, "")
	}
	for e := range extras {
		if evalMarker(req.marker, e) {
			return true
		}
	}
	return evalMarker(req.marker, "")
}

func evalMarker(marker, extra string) bool {
	p := &markerParser{toks: tokenize(marker), extra: extra}
	v := p.or()
	return v
}

type markerParser struct {
	toks  []string
	pos   int
	extra string
}

func (p *markerParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *markerParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *markerParser) or() bool {
	v := p.and()
	for p.peek() == "or" {
		p.next()
		w := p.and()
		v = v || w
	}
	return v
}

func (p *markerParser) and() bool {
	v := p.primary()
	for p.peek() == "and" {
		p.next()
		w := p.primary()
		v = v && w
	}
	return v
}

func (p *markerParser) primary() bool {
	if p.peek() == "(" {
		p.next()
		v := p.or()
		p.next() // ")"
		return v
	}
	left := p.value()
	op := p.next()
	if op == "not" {
		p.next() // "in"
		op = "not in"
	}
	right := p.value()
	return compare(left, op, right)
}

func (p *markerParser) value() string {
	t := p.next()
	if strings.HasPrefix(t, "'") || strings.HasPrefix(t, "\"") {
		return t[1 : len(t)-1]
	}
	if t == "extra" {
		return p.extra
	}
	return markerEnv[t]
}

func compare(a, op, b string) bool {
	if isVersion(a) && isVersion(b) {
		c := compareVersions(a, b)
		switch op {
		case "==", "===":
			return c == 0
		case "!=":
			return c != 0
		case "<":
			return c < 0
		case "<=":
			return c <= 0
		case ">":
			return c > 0
		case ">=":
			return c >= 0
		case "~=":
			return c >= 0
		}
	}
	switch op {
	case "==", "===":
		return normalize(a) == normalize(b)
	case "!=":
		return normalize(a) != normalize(b)
	case "in":
		return strings.Contains(b, a)
	case "not in":
		return !strings.Contains(b, a)
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

var versionRe = regexp.MustCompile(`^\d+(\.\d+)*$`)

func isVersion(s string) bool { return versionRe.MatchString(s) }

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func tokenize(s string) []string {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')':
			toks = append(toks, string(c))
			i++
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				j = len(s) - i - 1
			}
			toks = append(toks, s[i:i+j+2])
			i += j + 2
		case strings.ContainsRune("<>=!~", rune(c)):
			j := i
			for j < len(s) && strings.ContainsRune("<>=!~", rune(s[j])) {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		default:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '.' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j == i {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
