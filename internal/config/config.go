// Package config loads forge-scout's configuration from one YAML file (default
// /config/config.yaml, CONFIG_PATH overrides): global cadence and logging, and
// the list of forge connections. A string value may reference an environment
// variable whose name ends in _TOKEN or starts with FORGE_SCOUT_ as ${NAME},
// so secrets stay in the environment. The decode is strict: an unknown key or
// a second document is an error.
//
// This package never logs: every non-fatal problem is returned as a Warning
// for the caller to emit. No error or warning carries a value expanded from
// the environment (see source.show).
package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/envx/yamlenv/v2"
	"github.com/cplieger/forgeapi"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/slogx"
	"go.yaml.in/yaml/v3"
)

// defaultPath is the container-internal config file path.
const defaultPath = "/config/config.yaml"

// Defaults and clamp bounds for the global keys.
const (
	DefaultScanInterval = 15 * time.Minute
	defaultLookback     = 72 * time.Hour
	minScanInterval     = time.Minute
	minLookback         = time.Hour
	maxLookback         = 720 * time.Hour
	// maxScanInterval is the longest whole hour the longest lookback still
	// covers (see overlap).
	maxScanInterval = 171 * time.Hour
	// maxConfigBytes bounds the read of a small document.
	maxConfigBytes = 1 << 20
)

// connectionName is the operator label, emitted as the connection field and
// used as a state key, so it is kept to a short, unambiguous token.
var connectionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Config is the effective runtime configuration.
type Config struct {
	Connections  []Connection
	ScanInterval time.Duration
	Lookback     time.Duration
	LogLevel     slog.Level
}

// Connection is one configured forge instance. The four sets hold lowercased
// entries: repository paths, author logins, label names, bot logins.
type Connection struct {
	ExcludeRepos     map[string]bool
	ExcludeAuthors   map[string]bool
	ExcludeLabels    map[string]bool
	BotAuthors       map[string]bool
	Name             string
	URL              string
	APIURL           string
	Token            string
	Security         Security
	Owners           []string
	PrivateAddresses bool
	AllowPlaintext   bool
	// SecuritySet reports a security key written explicitly, which the
	// collector warns about on a product without code scanning.
	SecuritySet bool
}

// Security configures the code-scanning read.
type Security struct {
	// SkipRepos holds lowercased repository paths whose alerts are not read.
	SkipRepos map[string]bool
	Enabled   bool
	SkipForks bool
	// IncludePrivate reads private repositories too, which GitHub answers
	// only with Advanced Security on.
	IncludePrivate bool
}

// Warning is a non-fatal configuration message and its slog attributes.
type Warning struct {
	Msg   string
	Attrs []slog.Attr
}

// source is the document's scalar values as written, before expansion, by
// key path, such as connections[1].owners[2].
type source map[string]string

type fileConfig struct {
	ScanInterval string           `yaml:"scan_interval"`
	Lookback     string           `yaml:"lookback"`
	LogLevel     string           `yaml:"log_level"`
	Connections  []fileConnection `yaml:"connections"`
}

type fileConnection struct {
	Security         *fileSecurity `yaml:"security"`
	Name             string        `yaml:"name"`
	URL              string        `yaml:"url"`
	APIURL           string        `yaml:"api_url"`
	Token            string        `yaml:"token"`
	Owners           []string      `yaml:"owners"`
	ExcludeRepos     []string      `yaml:"exclude_repos"`
	ExcludeAuthors   []string      `yaml:"exclude_authors"`
	ExcludeLabels    []string      `yaml:"exclude_labels"`
	BotAuthors       []string      `yaml:"bot_authors"`
	PrivateAddresses bool          `yaml:"private_addresses"`
	AllowPlaintext   bool          `yaml:"allow_plaintext"`
}

type fileSecurity struct {
	Enabled        *bool    `yaml:"enabled"`
	SkipForks      *bool    `yaml:"skip_forks"`
	IncludePrivate *bool    `yaml:"include_private"`
	SkipRepos      []string `yaml:"skip_repos"`
}

// Path returns CONFIG_PATH as set, or defaultPath when it is unset or empty.
func Path() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return defaultPath
}

// Load reads, expands and validates the file at path. A missing file is an
// error wrapping os.ErrNotExist. Every validation problem is joined into the
// one error returned.
func Load(path string) (Config, []Warning, error) {
	fc, src, unresolved, err := decode(path)
	if err != nil {
		return Config{}, nil, err
	}
	var warns []Warning
	cfg := Config{}
	cfg.ScanInterval, warns = src.duration(warns, "scan_interval", fc.ScanInterval, DefaultScanInterval, minScanInterval, maxScanInterval)
	cfg.Lookback, warns = src.duration(warns, "lookback", fc.Lookback, defaultLookback, minLookback, maxLookback)
	lvl, ok := slogx.ParseLevel(fc.LogLevel, slog.LevelInfo)
	if !ok {
		warns = append(warns, Warning{Msg: "invalid log_level, using default", Attrs: []slog.Attr{slog.String("default", "info")}})
	}
	cfg.LogLevel = lvl
	conns, errs := connections(fc.Connections, src)
	if err := src.overlap(cfg.ScanInterval, cfg.Lookback); err != nil {
		errs = append(errs, err)
	}
	// Every product line, scan complete included, is an INFO record.
	if lvl > slog.LevelInfo {
		errs = append(errs, fmt.Errorf("log_level %s would drop the lines forge-scout emits at info, so use debug or info", src.show("log_level", lvl.String())))
	}
	if len(errs) > 0 {
		return Config{}, nil, fmt.Errorf("config %q: %w", path, errors.Join(errs...))
	}
	cfg.Connections = conns
	if names := unresolvedOutsideTokens(unresolved, fc.Connections); len(names) > 0 {
		warns = append(warns, Warning{
			Msg:   "config references unset environment variables",
			Attrs: []slog.Attr{slog.String("names", strings.Join(names, ","))},
		})
	}
	return cfg, warns, nil
}

// ScanLimit is the longest one connection's scan may run at a scan interval:
// twice the interval.
func ScanLimit(interval time.Duration) time.Duration { return 2 * interval }

// ScanJitterPercent is how much each wait between two scans may differ from
// scan_interval, as a percentage.
const ScanJitterPercent = 10

// ScanJitter is ScanJitterPercent as the fraction the scan loop draws each
// wait with (scheduler.LoopOptions.Jitter).
const ScanJitter = ScanJitterPercent / 100.0

// maxWait bounds the scan loop's wait between two scans at interval.
func maxWait(interval time.Duration) time.Duration {
	return interval + interval*ScanJitterPercent/100
}

// Instance is the forge c.URL names, in one spelling for every equivalent
// URL: scheme and host lowercased, a default port and a trailing slash
// dropped. Every URL on a GitHub.com host (forge.GitHubDotcom) is
// https://github.com, as its API is one whatever the scheme or path. The
// path of any other instance is kept, since it can name another instance.
func (c *Connection) Instance() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return strings.ToLower(c.URL)
	}
	if forge.GitHubDotcom(u.Hostname()) {
		return "https://github.com"
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	return scheme + "://" + host + path
}

// overlap refuses a lookback shorter than two of the scan loop's longest
// waits and one scan at its limit: every scan lists the lookback window, so
// each listing must reach back past the start of the one before it even
// after one scan is missed and the next runs to its limit, or runs created
// between them go unlisted.
func (s source) overlap(interval, lookback time.Duration) error {
	need := 2*maxWait(interval) + ScanLimit(interval)
	if lookback >= need {
		return nil
	}
	shown := s.show("lookback", lookback.String())
	// Every length below derives from the interval, so an expanded one
	// leaves the rule alone.
	if s.expanded("scan_interval") {
		return fmt.Errorf("lookback %s is shorter than two scan_interval %s each up to %d%% late plus one scan at its limit of twice scan_interval, "+
			"and every scan lists the lookback window, so raise lookback or lower scan_interval",
			shown, s["scan_interval"], ScanJitterPercent)
	}
	return fmt.Errorf("lookback %s is shorter than %s, which is two scan_interval %s each up to %d%% late plus one scan at its %s limit, "+
		"and every scan lists the lookback window, so set lookback to at least %s or lower scan_interval",
		shown, need, interval, ScanJitterPercent, ScanLimit(interval), need)
}

// ScanIntervalFromFile returns the file's effective scan interval, or the
// default on any error, without reporting anything: it serves the health probe.
func ScanIntervalFromFile(path string) time.Duration {
	cfg, _, err := Load(path)
	if err != nil {
		return DefaultScanInterval
	}
	return cfg.ScanInterval
}

// decode reads and expands the file, and returns its values as written.
func decode(path string) (fc fileConfig, src source, unresolved []string, err error) {
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && legacyEnvSet() {
			return fileConfig{}, nil, nil, fmt.Errorf("read config %q: %w. forge-scout reads its settings from this file, not from environment variables such as GITHUB_OWNER. See the README", path, err)
		}
		return fileConfig{}, nil, nil, fmt.Errorf("read config %q: %w", path, err)
	}
	unresolved, err = yamlenv.Load(raw, &fc, allowedEnv, yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho(true)))
	if err != nil {
		return fileConfig{}, nil, nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fileConfig{}, nil, nil, fmt.Errorf("parse config %q: %w", path, yamlenv.SanitizeDecodeError(err))
	}
	if where, ok := tokenRefOutsideToken(&doc); ok {
		return fileConfig{}, nil, nil, fmt.Errorf("config %q: %s: a token variable is referenced outside token, where it would be logged", path, where)
	}
	return fc, sourceOf(&doc), unresolved, nil
}

func sourceOf(doc *yaml.Node) source {
	s := source{}
	scalars(doc, "", false, func(path, value string, _ bool) bool {
		s[path] = value
		return true
	})
	return s
}

// show is how every error and warning names the value at path: as the file
// wrote it where it holds a ${NAME} reference, since the expanded value may
// be a secret, and as value otherwise.
func (s source) show(path, value string) string {
	if s.expanded(path) {
		return s[path]
	}
	return value
}

// expanded reports a value at path written with a ${NAME} reference.
func (s source) expanded(path string) bool { return varRef.MatchString(s[path]) }

// connectionPath is the key path of the connection at index.
func connectionPath(index int) string { return "connections[" + strconv.Itoa(index+1) + "]" }

// varRef matches a ${NAME} reference, in yamlenv's reference grammar.
var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// tokenRef matches a token scalar that is one reference and nothing else.
var tokenRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// tokenRefOutsideToken reports where the raw document, before expansion,
// references a token variable in a value other than a connection's token:
// every other value can reach a log line or an error, so a token there would
// leak. A token variable is one whose name ends in _TOKEN, or an allowed one a
// token references. It reads references, never a variable's value.
func tokenRefOutsideToken(doc *yaml.Node) (where string, found bool) {
	tokenVars := map[string]bool{}
	scalars(doc, "", false, func(_, value string, token bool) bool {
		for _, m := range varRef.FindAllStringSubmatch(value, -1) {
			if token && allowedEnv(m[1]) {
				tokenVars[m[1]] = true
			}
		}
		return true
	})
	found = !scalars(doc, "", false, func(path, value string, token bool) bool {
		for _, m := range varRef.FindAllStringSubmatch(value, -1) {
			if !token && (strings.HasSuffix(m[1], "_TOKEN") || tokenVars[m[1]]) {
				where = cmp.Or(path, "the document")
				return false
			}
		}
		return true
	})
	return where, found
}

// scalars calls visit with every scalar value under n, its path, and whether
// it is a connection's token, until visit returns false; it reports whether
// the walk ran to the end.
func scalars(n *yaml.Node, path string, tokenField bool, visit func(path, value string, token bool) bool) bool {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return visit(path, n.Value, tokenField)
	case yaml.MappingNode:
		return mappingScalars(n, path, visit)
	}
	for i, child := range n.Content {
		childPath := path
		if n.Kind == yaml.SequenceNode {
			childPath = path + "[" + strconv.Itoa(i+1) + "]"
		}
		if !scalars(child, childPath, false, visit) {
			return false
		}
	}
	return true
}

// mappingScalars walks a mapping's values; only the token key of a
// connections entry is a token.
func mappingScalars(n *yaml.Node, path string, visit func(path, value string, token bool) bool) bool {
	connection := strings.HasPrefix(path, "connections[") && !strings.Contains(path, ".")
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if !scalars(n.Content[i+1], strings.TrimPrefix(path+"."+key, "."), connection && key == "token", visit) {
			return false
		}
	}
	return true
}

// readFile reads through an os.Root over the file's directory, so the open
// follows no symlink out of it and cannot block on a FIFO.
func readFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, _, err := atomicfile.OpenRegularInRoot(root, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return atomicfile.ReadBoundedFile(context.Background(), f, maxConfigBytes)
}

// legacyEnvSet reports GITHUB_OWNER or GITHUB_TOKEN, the variables the
// environment-configured releases read, so a missing file beside one names
// the move to a file.
func legacyEnvSet() bool { return os.Getenv("GITHUB_OWNER") != "" || os.Getenv("GITHUB_TOKEN") != "" }

func allowedEnv(name string) bool {
	return strings.HasSuffix(name, "_TOKEN") || strings.HasPrefix(name, "FORGE_SCOUT_")
}

// duration parses one global duration key.
func (s source) duration(warns []Warning, key, raw string, def, lo, hi time.Duration) (time.Duration, []Warning) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, warns
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def, append(warns, Warning{
			Msg:   "invalid " + key + ", using default",
			Attrs: []slog.Attr{slog.String("key", key), slog.String("default", def.String())},
		})
	}
	clamped := min(max(d, lo), hi)
	if clamped != d {
		warns = append(warns, Warning{
			Msg:   "config value clamped",
			Attrs: []slog.Attr{slog.String("key", key), slog.String("requested", s.show(key, d.String())), slog.String("clamped_to", clamped.String())},
		})
	}
	return clamped, warns
}

func connections(in []fileConnection, src source) ([]Connection, []error) {
	if len(in) == 0 {
		return nil, []error{errors.New("the file declares no connection, and at least one connection is required")}
	}
	var errs []error
	seen := make(map[string]bool, len(in))
	out := make([]Connection, 0, len(in))
	for i := range in {
		c, cerrs := connection(&in[i], i, src)
		errs = append(errs, cerrs...)
		if c.Name != "" && seen[c.Name] {
			errs = append(errs, fmt.Errorf("connection %q is named twice", src.show(connectionPath(i)+".name", c.Name)))
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out, errs
}

func connection(fc *fileConnection, index int, src source) (Connection, []error) {
	name := strings.TrimSpace(fc.Name)
	at := connectionPath(index)
	label := fmt.Sprintf("connection %d", index+1)
	var errs []error
	if !connectionName.MatchString(name) {
		errs = append(errs, fmt.Errorf("%s: name must match %s", label, connectionName))
	} else {
		label = fmt.Sprintf("connection %q", src.show(at+".name", name))
	}
	c := Connection{
		Name:             name,
		URL:              strings.TrimRight(strings.TrimSpace(fc.URL), "/"),
		APIURL:           strings.TrimRight(strings.TrimSpace(fc.APIURL), "/"),
		Token:            fc.Token,
		PrivateAddresses: fc.PrivateAddresses,
		AllowPlaintext:   fc.AllowPlaintext,
		ExcludeAuthors:   lowerSet(fc.ExcludeAuthors),
		ExcludeLabels:    lowerSet(fc.ExcludeLabels),
		BotAuthors:       lowerSet(fc.BotAuthors),
	}
	errs = append(errs, checkURL(label, "url", c.URL, true, c.AllowPlaintext)...)
	errs = append(errs, checkURL(label, "api_url", c.APIURL, false, c.AllowPlaintext)...)
	errs = append(errs, checkToken(label, c.Token, src[at+".token"])...)
	var oerrs []error
	c.Owners, oerrs = owners(label, at+".owners", fc.Owners, src)
	errs = append(errs, oerrs...)
	var rerr error
	if c.ExcludeRepos, rerr = repoSet(label, "exclude_repos", fc.ExcludeRepos); rerr != nil {
		errs = append(errs, rerr)
	}
	sec, serr := security(label, fc.Security)
	if serr != nil {
		errs = append(errs, serr)
	}
	c.Security, c.SecuritySet = sec, fc.Security != nil
	return c, errs
}

func checkURL(label, key, raw string, required, plaintext bool) []error {
	if raw == "" {
		if required {
			return []error{fmt.Errorf("%s: %s is required", label, key)}
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || !validPort(u.Port()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return []error{fmt.Errorf("%s: %s must be an absolute http(s) URL with a host, a port from 1 to 65535 if any, and no credentials, query or fragment", label, key)}
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !plaintext {
			return []error{fmt.Errorf("%s: %s uses http, which needs allow_plaintext: true", label, key)}
		}
		return nil
	default:
		return []error{fmt.Errorf("%s: %s must use https or http", label, key)}
	}
}

// validPort reports an absent port or one from 1 to 65535.
func validPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.ParseUint(p, 10, 16)
	return err == nil && n > 0
}

// checkToken refuses a token not written as exactly one ${NAME} reference
// (raw, before expansion), one naming a variable that is not allowed, unset
// or blank. An error names the variable, never a value. The expanded token
// is kept verbatim: the forge verifies its exact bytes.
func checkToken(label, token, raw string) []error {
	if strings.TrimSpace(raw) == "" {
		return []error{fmt.Errorf("%s: token is required, as a ${NAME} reference such as ${GITHUB_TOKEN}", label)}
	}
	m := tokenRef.FindStringSubmatch(raw)
	switch {
	case m == nil:
		return []error{fmt.Errorf("%s: token must be one ${NAME} reference and nothing else, such as ${GITHUB_TOKEN}: the token's value comes from the environment, never from this file", label)}
	case !allowedEnv(m[1]):
		return []error{fmt.Errorf("%s: token references %s, which is not an allowed name (it must end in _TOKEN or start with FORGE_SCOUT_)", label, m[1])}
	case token == raw:
		return []error{fmt.Errorf("%s: token references %s, which is not set", label, m[1])}
	case strings.TrimSpace(token) == "":
		return []error{fmt.Errorf("%s: token references %s, which is empty", label, m[1])}
	}
	return nil
}

func owners(label, at string, in []string, src source) ([]string, []error) {
	if len(in) == 0 {
		return nil, []error{fmt.Errorf("%s: at least one owner is required", label)}
	}
	var errs []error
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for i, raw := range in {
		o := strings.TrimSpace(raw)
		if err := forgeapi.ValidateOwner(forgeapi.FamilyGitLab, o); err != nil {
			errs = append(errs, fmt.Errorf("%s: owner %d is not a valid owner name or group path", label, i+1))
			continue
		}
		key := strings.ToLower(o)
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: owner %q is listed twice", label, src.show(at+"["+strconv.Itoa(i+1)+"]", o)))
			continue
		}
		seen[key] = true
		out = append(out, o)
	}
	return out, errs
}

// repoSet lowercases full repository paths. A bare name is refused: the same
// name can exist under several owners of one connection.
func repoSet(label, key string, in []string) (map[string]bool, error) {
	out := make(map[string]bool, len(in))
	for _, raw := range in {
		p := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "/"))
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			return nil, fmt.Errorf("%s: %s entries are full owner/name paths", label, key)
		}
		out[p] = true
	}
	return out, nil
}

func security(label string, fs *fileSecurity) (Security, error) {
	s := Security{Enabled: true, SkipForks: true, SkipRepos: map[string]bool{}}
	if fs == nil {
		return s, nil
	}
	if fs.Enabled != nil {
		s.Enabled = *fs.Enabled
	}
	if fs.SkipForks != nil {
		s.SkipForks = *fs.SkipForks
	}
	if fs.IncludePrivate != nil {
		s.IncludePrivate = *fs.IncludePrivate
	}
	skip, err := repoSet(label, "security.skip_repos", fs.SkipRepos)
	if err != nil {
		return s, err
	}
	s.SkipRepos = skip
	return s, nil
}

func lowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, raw := range in {
		if v := strings.ToLower(strings.TrimSpace(raw)); v != "" {
			out[v] = true
		}
	}
	return out
}

// unresolvedOutsideTokens returns the unresolved names no token references;
// an unresolved token is already a load error.
func unresolvedOutsideTokens(unresolved []string, conns []fileConnection) []string {
	var out []string
	for _, name := range unresolved {
		inToken := false
		for i := range conns {
			if strings.Contains(conns[i].Token, "${"+name+"}") {
				inToken = true
				break
			}
		}
		if !inToken {
			out = append(out, name)
		}
	}
	return out
}
