// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"go.uber.org/zap"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

type (
	// MatchHost matches requests by the Host value (case-insensitive).
	//
	// When used in a top-level HTTP route,
	// [qualifying domain names](/docs/automatic-https#hostname-requirements)
	// may trigger [automatic HTTPS](/docs/automatic-https), which automatically
	// provisions and renews certificates for you. Before doing this, you
	// should ensure that DNS records for these domains are properly configured,
	// especially A/AAAA pointed at your server.
	//
	// Automatic HTTPS can be
	// [customized or disabled](/docs/modules/http#servers/automatic_https).
	//
	// Wildcards (`*`) may be used to represent exactly one label of the
	// hostname, in accordance with RFC 1034 (because host matchers are also
	// used for automatic HTTPS which influences TLS certificates). Thus,
	// a host of `*` matches hosts like `localhost` or `internal` but not
	// `example.com`. To catch all hosts, omit the host matcher entirely.
	//
	// The wildcard can be useful for matching all subdomains, for example:
	// `*.example.com` matches `foo.example.com` but not `foo.bar.example.com`.
	//
	// Duplicate entries will return an error.
	MatchHost []string

	// MatchPath matches requests by the URI's path (case-insensitive). Path
	// matches are exact, but wildcards may be used:
	//
	// - At the end, for a prefix match (`/prefix/*`)
	// - At the beginning, for a suffix match (`*.suffix`)
	// - On both sides, for a substring match (`*/contains/*`)
	// - In the middle, for a globular match (`/accounts/*/info`)
	//
	// This matcher is fast, so it does not support regular expressions or
	// capture groups. For slower but more powerful matching, use the
	// path_regexp matcher.
	MatchPath []string

	// MatchPathRE matches requests by a regular expression on the URI's path.
	//
	// Upon a match, it adds placeholders to the request: `{http.regexp.name.capture_group}`
	// where `name` is the regular expression's name, and `capture_group` is either
	// the named or positional capture group from the expression itself. If no name
	// is given, then the placeholder omits the name: `{http.regexp.capture_group}`
	// (potentially leading to collisions).
	MatchPathRE struct{ MatchRegexp }

	// MatchMethod matches requests by the method.
	MatchMethod []string

	// MatchQuery matches requests by the URI's query string. It takes a JSON object
	// keyed by the query keys, with an array of string values to match for that key.
	// Query key matches are exact, but wildcards may be used for value matches. Both
	// keys and values may be placeholders.
	// An example of the structure to match `?key=value&topic=api&query=something` is:
	//
	// ```json
	// {
	// 	"key": ["value"],
	//	"topic": ["api"],
	//	"query": ["*"]
	// }
	// ```
	//
	// Invalid query strings, including those with bad escapings or illegal characters
	// like semicolons, will fail to parse and thus fail to match.
	MatchQuery url.Values

	// MatchHeader matches requests by header fields. The key is the field
	// name and the array is the list of field values. It performs fast,
	// exact string comparisons of the field values. Fast prefix, suffix,
	// and substring matches can also be done by suffixing, prefixing, or
	// surrounding the value with the wildcard `*` character, respectively.
	// If a list is null, the header must not exist. If the list is empty,
	// the field must simply exist, regardless of its value.
	MatchHeader http.Header

	// MatchHeaderRE matches requests by a regular expression on header fields.
	//
	// Upon a match, it adds placeholders to the request: `{http.regexp.name.capture_group}`
	// where `name` is the regular expression's name, and `capture_group` is either
	// the named or positional capture group from the expression itself. If no name
	// is given, then the placeholder omits the name: `{http.regexp.capture_group}`
	// (potentially leading to collisions).
	MatchHeaderRE map[string]*MatchRegexp

	// MatchProtocol matches requests by protocol. Recognized values are
	// "http", "https", and "grpc".
	MatchProtocol string

	// MatchRemoteIP matches requests by client IP (or CIDR range).
	MatchRemoteIP struct {
		// The IPs or CIDR ranges to match.
		Ranges []string `json:"ranges,omitempty"`

		// If true, prefer the first IP in the request's X-Forwarded-For
		// header, if present, rather than the immediate peer's IP, as
		// the reference IP against which to match. Note that it is easy
		// to spoof request headers. Default: false
		Forwarded bool `json:"forwarded,omitempty"`

		// cidrs and zones vars should aligned always in the same
		// length and indexes for matching later
		cidrs  []*net.IPNet
		zones  []string
		logger *zap.Logger
	}

	// MatchNot matches requests by negating the results of its matcher
	// sets. A single "not" matcher takes one or more matcher sets. Each
	// matcher set is OR'ed; in other words, if any matcher set returns
	// true, the final result of the "not" matcher is false. Individual
	// matchers within a set work the same (i.e. different matchers in
	// the same set are AND'ed).
	//
	// NOTE: The generated docs which describe the structure of this
	// module are wrong because of how this type unmarshals JSON in a
	// custom way. The correct structure is:
	//
	// ```json
	// [
	// 	{},
	// 	{}
	// ]
	// ```
	//
	// where each of the array elements is a matcher set, i.e. an
	// object keyed by matcher name.
	MatchNot struct {
		MatcherSetsRaw []caddy.ModuleMap `json:"-" caddy:"namespace=http.matchers"`
		MatcherSets    []MatcherSet      `json:"-"`
	}
)

func init() {
	caddy.RegisterModule(MatchHost{})
	caddy.RegisterModule(MatchPath{})
	caddy.RegisterModule(MatchPathRE{})
	caddy.RegisterModule(MatchMethod{})
	caddy.RegisterModule(MatchQuery{})
	caddy.RegisterModule(MatchHeader{})
	caddy.RegisterModule(MatchHeaderRE{})
	caddy.RegisterModule(new(MatchProtocol))
	caddy.RegisterModule(MatchRemoteIP{})
	caddy.RegisterModule(MatchNot{})
}

// CaddyModule returns the Caddy module information.
func (MatchHost) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.host",
		New: func() caddy.Module { return new(MatchHost) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchHost) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		*m = append(*m, d.RemainingArgs()...)
		if d.NextBlock(0) {
			return d.Err("malformed host matcher: blocks are not supported")
		}
	}
	return nil
}

// Provision sets up and validates m, including making it more efficient for large lists.
func (m MatchHost) Provision(_ caddy.Context) error {
	// check for duplicates; they are nonsensical and reduce efficiency
	// (we could just remove them, but the user should know their config is erroneous)
	seen := make(map[string]int)
	for i, h := range m {
		h = strings.ToLower(h)
		if firstI, ok := seen[h]; ok {
			return fmt.Errorf("host at index %d is repeated at index %d: %s", firstI, i, h)
		}
		seen[h] = i
	}

	if m.large() {
		// sort the slice lexicographically, grouping "fuzzy" entries (wildcards and placeholders)
		// at the front of the list; this allows us to use binary search for exact matches, which
		// we have seen from experience is the most common kind of value in large lists; and any
		// other kinds of values (wildcards and placeholders) are grouped in front so the linear
		// search should find a match fairly quickly
		sort.Slice(m, func(i, j int) bool {
			iInexact, jInexact := m.fuzzy(m[i]), m.fuzzy(m[j])
			if iInexact && !jInexact {
				return true
			}
			if !iInexact && jInexact {
				return false
			}
			return m[i] < m[j]
		})
	}

	return nil
}

// Match returns true if r matches m.
func (m MatchHost) Match(r *http.Request) bool {
	reqHost, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// OK; probably didn't have a port
		reqHost = r.Host

		// make sure we strip the brackets from IPv6 addresses
		reqHost = strings.TrimPrefix(reqHost, "[")
		reqHost = strings.TrimSuffix(reqHost, "]")
	}

	if m.large() {
		// fast path: locate exact match using binary search (about 100-1000x faster for large lists)
		pos := sort.Search(len(m), func(i int) bool {
			if m.fuzzy(m[i]) {
				return false
			}
			return m[i] >= reqHost
		})
		if pos < len(m) && m[pos] == reqHost {
			return true
		}
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

outer:
	for _, host := range m {
		// fast path: if matcher is large, we already know we don't have an exact
		// match, so we're only looking for fuzzy match now, which should be at the
		// front of the list; if we have reached a value that is not fuzzy, there
		// will be no match and we can short-circuit for efficiency
		if m.large() && !m.fuzzy(host) {
			break
		}

		host = repl.ReplaceAll(host, "")
		if strings.Contains(host, "*") {
			patternParts := strings.Split(host, ".")
			incomingParts := strings.Split(reqHost, ".")
			if len(patternParts) != len(incomingParts) {
				continue
			}
			for i := range patternParts {
				if patternParts[i] == "*" {
					continue
				}
				if !strings.EqualFold(patternParts[i], incomingParts[i]) {
					continue outer
				}
			}
			return true
		} else if strings.EqualFold(reqHost, host) {
			return true
		}
	}

	return false
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression host('localhost')
func (MatchHost) CELLibrary(ctx caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		"host",
		"host_match_request_list",
		[]*exprpb.Type{CelTypeListString},
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			strList, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			matcher := MatchHost(strList.([]string))
			err = matcher.Provision(ctx)
			return matcher, err
		},
	)
}

// fuzzy returns true if the given hostname h is not a specific
// hostname, e.g. has placeholders or wildcards.
func (MatchHost) fuzzy(h string) bool { return strings.ContainsAny(h, "{*") }

// large returns true if m is considered to be large. Optimizing
// the matcher for smaller lists has diminishing returns.
// See related benchmark function in test file to conduct experiments.
func (m MatchHost) large() bool { return len(m) > 100 }

// CaddyModule returns the Caddy module information.
func (MatchPath) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.path",
		New: func() caddy.Module { return new(MatchPath) },
	}
}

// Provision lower-cases the paths in m to ensure case-insensitive matching.
func (m MatchPath) Provision(_ caddy.Context) error {
	for i := range m {
		m[i] = strings.ToLower(m[i])
	}
	return nil
}

// Match returns true if r matches m.
func (m MatchPath) Match(r *http.Request) bool {
	// PathUnescape returns an error if the escapes aren't
	// well-formed, meaning the count % matches the RFC.
	// Return early if the escape is improper.
	unescapedPath, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		return false
	}

	lowerPath := strings.ToLower(unescapedPath)

	// Clean the path, merges doubled slashes, etc.
	// This ensures maliciously crafted requests can't bypass
	// the path matcher. See #4407
	lowerPath = path.Clean(lowerPath)

	// see #2917; Windows ignores trailing dots and spaces
	// when accessing files (sigh), potentially causing a
	// security risk (cry) if PHP files end up being served
	// as static files, exposing the source code, instead of
	// being matched by *.php to be treated as PHP scripts
	lowerPath = strings.TrimRight(lowerPath, ". ")

	// Cleaning may remove the trailing slash, but we want to keep it
	if lowerPath != "/" && strings.HasSuffix(r.URL.Path, "/") {
		lowerPath = lowerPath + "/"
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	for _, matchPath := range m {
		matchPath = repl.ReplaceAll(matchPath, "")

		// special case: whole path is wildcard; this is unnecessary
		// as it matches all requests, which is the same as no matcher
		if matchPath == "*" {
			return true
		}

		// special case: first and last characters are wildcard,
		// treat it as a fast substring match
		if len(matchPath) > 1 &&
			strings.HasPrefix(matchPath, "*") &&
			strings.HasSuffix(matchPath, "*") {
			if strings.Contains(lowerPath, matchPath[1:len(matchPath)-1]) {
				return true
			}
			continue
		}

		// special case: first character is a wildcard,
		// treat it as a fast suffix match
		if strings.HasPrefix(matchPath, "*") {
			if strings.HasSuffix(lowerPath, matchPath[1:]) {
				return true
			}
			continue
		}

		// special case: last character is a wildcard,
		// treat it as a fast prefix match
		if strings.HasSuffix(matchPath, "*") {
			if strings.HasPrefix(lowerPath, matchPath[:len(matchPath)-1]) {
				return true
			}
			continue
		}

		// for everything else, try globular matching, which also
		// is exact matching if there are no glob/wildcard chars;
		// can ignore error here because we can't handle it anyway
		matches, _ := filepath.Match(matchPath, lowerPath)
		if matches {
			return true
		}
	}
	return false
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression path('*substring*', '*suffix')
func (MatchPath) CELLibrary(ctx caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		// name of the macro, this is the function name that users see when writing expressions.
		"path",
		// name of the function that the macro will be rewritten to call.
		"path_match_request_list",
		// internal data type of the MatchPath value.
		[]*exprpb.Type{CelTypeListString},
		// function to convert a constant list of strings to a MatchPath instance.
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			strList, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			matcher := MatchPath(strList.([]string))
			err = matcher.Provision(ctx)
			return matcher, err
		},
	)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchPath) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		*m = append(*m, d.RemainingArgs()...)
		if d.NextBlock(0) {
			return d.Err("malformed path matcher: blocks are not supported")
		}
	}
	return nil
}

// CaddyModule returns the Caddy module information.
func (MatchPathRE) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.path_regexp",
		New: func() caddy.Module { return new(MatchPathRE) },
	}
}

// Match returns true if r matches m.
func (m MatchPathRE) Match(r *http.Request) bool {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// PathUnescape returns an error if the escapes aren't
	// well-formed, meaning the count % matches the RFC.
	// Return early if the escape is improper.
	unescapedPath, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		return false
	}

	// Clean the path, merges doubled slashes, etc.
	// This ensures maliciously crafted requests can't bypass
	// the path matcher. See #4407
	cleanedPath := path.Clean(unescapedPath)

	// Cleaning may remove the trailing slash, but we want to keep it
	if cleanedPath != "/" && strings.HasSuffix(r.URL.Path, "/") {
		cleanedPath = cleanedPath + "/"
	}

	return m.MatchRegexp.Match(cleanedPath, repl)
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression path_regexp('^/bar')
func (MatchPathRE) CELLibrary(ctx caddy.Context) (cel.Library, error) {
	unnamedPattern, err := CELMatcherImpl(
		"path_regexp",
		"path_regexp_request_string",
		[]*exprpb.Type{decls.String},
		func(data ref.Val) (RequestMatcher, error) {
			pattern := data.(types.String)
			matcher := MatchPathRE{MatchRegexp{Pattern: string(pattern)}}
			err := matcher.Provision(ctx)
			return matcher, err
		},
	)
	if err != nil {
		return nil, err
	}
	namedPattern, err := CELMatcherImpl(
		"path_regexp",
		"path_regexp_request_string_string",
		[]*exprpb.Type{decls.String, decls.String},
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			params, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			strParams := params.([]string)
			matcher := MatchPathRE{MatchRegexp{Name: strParams[0], Pattern: strParams[1]}}
			err = matcher.Provision(ctx)
			return matcher, err
		},
	)
	if err != nil {
		return nil, err
	}
	envOpts := append(unnamedPattern.CompileOptions(), namedPattern.CompileOptions()...)
	prgOpts := append(unnamedPattern.ProgramOptions(), namedPattern.ProgramOptions()...)
	return NewMatcherCELLibrary(envOpts, prgOpts), nil
}

// CaddyModule returns the Caddy module information.
func (MatchMethod) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.method",
		New: func() caddy.Module { return new(MatchMethod) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchMethod) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		*m = append(*m, d.RemainingArgs()...)
		if d.NextBlock(0) {
			return d.Err("malformed method matcher: blocks are not supported")
		}
	}
	return nil
}

// Match returns true if r matches m.
func (m MatchMethod) Match(r *http.Request) bool {
	for _, method := range m {
		if r.Method == method {
			return true
		}
	}
	return false
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression method('PUT', 'POST')
func (MatchMethod) CELLibrary(_ caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		"method",
		"method_request_list",
		[]*exprpb.Type{CelTypeListString},
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			strList, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			return MatchMethod(strList.([]string)), nil
		},
	)
}

// CaddyModule returns the Caddy module information.
func (MatchQuery) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.query",
		New: func() caddy.Module { return new(MatchQuery) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchQuery) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if *m == nil {
		*m = make(map[string][]string)
	}
	for d.Next() {
		for _, query := range d.RemainingArgs() {
			if query == "" {
				continue
			}
			parts := strings.SplitN(query, "=", 2)
			if len(parts) != 2 {
				return d.Errf("malformed query matcher token: %s; must be in param=val format", d.Val())
			}
			url.Values(*m).Add(parts[0], parts[1])
		}
		if d.NextBlock(0) {
			return d.Err("malformed query matcher: blocks are not supported")
		}
	}
	return nil
}

// Match returns true if r matches m. An empty m matches an empty query string.
func (m MatchQuery) Match(r *http.Request) bool {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// parse query string just once, for efficiency
	parsed, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		// Illegal query string. Likely bad escape sequence or syntax.
		// Note that semicolons in query string have a controversial history. Summaries:
		// - https://github.com/golang/go/issues/50034
		// - https://github.com/golang/go/issues/25192
		// W3C recommendations are flawed and ambiguous, and different servers handle semicolons differently.
		// Filippo Valsorda rightly wrote: "Relying on parser alignment for security is doomed."
		return false
	}

	for param, vals := range m {
		param = repl.ReplaceAll(param, "")
		paramVal, found := parsed[param]
		if found {
			for _, v := range vals {
				v = repl.ReplaceAll(v, "")
				if paramVal[0] == v || v == "*" {
					return true
				}
			}
		}
	}
	return len(m) == 0 && len(r.URL.Query()) == 0
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression query({'sort': 'asc'}) || query({'foo': ['*bar*', 'baz']})
func (MatchQuery) CELLibrary(_ caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		"query",
		"query_matcher_request_map",
		[]*exprpb.Type{CelTypeJson},
		func(data ref.Val) (RequestMatcher, error) {
			mapStrListStr, err := CELValueToMapStrList(data)
			if err != nil {
				return nil, err
			}
			return MatchQuery(url.Values(mapStrListStr)), nil
		},
	)
}

// CaddyModule returns the Caddy module information.
func (MatchHeader) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.header",
		New: func() caddy.Module { return new(MatchHeader) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchHeader) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if *m == nil {
		*m = make(map[string][]string)
	}
	for d.Next() {
		var field, val string
		if !d.Args(&field) {
			return d.Errf("malformed header matcher: expected field")
		}

		if strings.HasPrefix(field, "!") {
			if len(field) == 1 {
				return d.Errf("malformed header matcher: must have field name following ! character")
			}

			field = field[1:]
			headers := *m
			headers[field] = nil
			m = &headers
			if d.NextArg() {
				return d.Errf("malformed header matcher: null matching headers cannot have a field value")
			}
		} else {
			if !d.NextArg() {
				return d.Errf("malformed header matcher: expected both field and value")
			}

			// If multiple header matchers with the same header field are defined,
			// we want to add the existing to the list of headers (will be OR'ed)
			val = d.Val()
			http.Header(*m).Add(field, val)
		}

		if d.NextBlock(0) {
			return d.Err("malformed header matcher: blocks are not supported")
		}
	}
	return nil
}

// Match returns true if r matches m.
func (m MatchHeader) Match(r *http.Request) bool {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	return matchHeaders(r.Header, http.Header(m), r.Host, repl)
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression header({'content-type': 'image/png'})
//    expression header({'foo': ['bar', 'baz']}) // match bar or baz
func (MatchHeader) CELLibrary(_ caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		"header",
		"header_matcher_request_map",
		[]*exprpb.Type{CelTypeJson},
		func(data ref.Val) (RequestMatcher, error) {
			mapStrListStr, err := CELValueToMapStrList(data)
			if err != nil {
				return nil, err
			}
			return MatchHeader(http.Header(mapStrListStr)), nil
		},
	)
}

// getHeaderFieldVals returns the field values for the given fieldName from input.
// The host parameter should be obtained from the http.Request.Host field since
// net/http removes it from the header map.
func getHeaderFieldVals(input http.Header, fieldName, host string) []string {
	fieldName = textproto.CanonicalMIMEHeaderKey(fieldName)
	if fieldName == "Host" && host != "" {
		return []string{host}
	}
	return input[fieldName]
}

// matchHeaders returns true if input matches the criteria in against without regex.
// The host parameter should be obtained from the http.Request.Host field since
// net/http removes it from the header map.
func matchHeaders(input, against http.Header, host string, repl *caddy.Replacer) bool {
	for field, allowedFieldVals := range against {
		actualFieldVals := getHeaderFieldVals(input, field, host)
		if allowedFieldVals != nil && len(allowedFieldVals) == 0 && actualFieldVals != nil {
			// a non-nil but empty list of allowed values means
			// match if the header field exists at all
			continue
		}
		if allowedFieldVals == nil && actualFieldVals == nil {
			// a nil list means match if the header does not exist at all
			continue
		}
		var match bool
	fieldVals:
		for _, actualFieldVal := range actualFieldVals {
			for _, allowedFieldVal := range allowedFieldVals {
				if repl != nil {
					allowedFieldVal = repl.ReplaceAll(allowedFieldVal, "")
				}
				switch {
				case allowedFieldVal == "*":
					match = true
				case strings.HasPrefix(allowedFieldVal, "*") && strings.HasSuffix(allowedFieldVal, "*"):
					match = strings.Contains(actualFieldVal, allowedFieldVal[1:len(allowedFieldVal)-1])
				case strings.HasPrefix(allowedFieldVal, "*"):
					match = strings.HasSuffix(actualFieldVal, allowedFieldVal[1:])
				case strings.HasSuffix(allowedFieldVal, "*"):
					match = strings.HasPrefix(actualFieldVal, allowedFieldVal[:len(allowedFieldVal)-1])
				default:
					match = actualFieldVal == allowedFieldVal
				}
				if match {
					break fieldVals
				}
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// CaddyModule returns the Caddy module information.
func (MatchHeaderRE) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.header_regexp",
		New: func() caddy.Module { return new(MatchHeaderRE) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchHeaderRE) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if *m == nil {
		*m = make(map[string]*MatchRegexp)
	}
	for d.Next() {
		var first, second, third string
		if !d.Args(&first, &second) {
			return d.ArgErr()
		}

		var name, field, val string
		if d.Args(&third) {
			name = first
			field = second
			val = third
		} else {
			field = first
			val = second
		}

		(*m)[field] = &MatchRegexp{Pattern: val, Name: name}

		if d.NextBlock(0) {
			return d.Err("malformed header_regexp matcher: blocks are not supported")
		}
	}
	return nil
}

// Match returns true if r matches m.
func (m MatchHeaderRE) Match(r *http.Request) bool {
	for field, rm := range m {
		actualFieldVals := getHeaderFieldVals(r.Header, field, r.Host)
		match := false
	fieldVal:
		for _, actualFieldVal := range actualFieldVals {
			repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
			if rm.Match(actualFieldVal, repl) {
				match = true
				break fieldVal
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// Provision compiles m's regular expressions.
func (m MatchHeaderRE) Provision(ctx caddy.Context) error {
	for _, rm := range m {
		err := rm.Provision(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

// Validate validates m's regular expressions.
func (m MatchHeaderRE) Validate() error {
	for _, rm := range m {
		err := rm.Validate()
		if err != nil {
			return err
		}
	}
	return nil
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression header_regexp('foo', 'Field', 'fo+')
func (MatchHeaderRE) CELLibrary(ctx caddy.Context) (cel.Library, error) {
	unnamedPattern, err := CELMatcherImpl(
		"header_regexp",
		"header_regexp_request_string_string",
		[]*exprpb.Type{decls.String, decls.String},
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			params, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			strParams := params.([]string)
			matcher := MatchHeaderRE{}
			matcher[strParams[0]] = &MatchRegexp{Pattern: strParams[1], Name: ""}
			err = matcher.Provision(ctx)
			return matcher, err
		},
	)
	if err != nil {
		return nil, err
	}
	namedPattern, err := CELMatcherImpl(
		"header_regexp",
		"header_regexp_request_string_string_string",
		[]*exprpb.Type{decls.String, decls.String, decls.String},
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			params, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}
			strParams := params.([]string)
			matcher := MatchHeaderRE{}
			matcher[strParams[1]] = &MatchRegexp{Pattern: strParams[2], Name: strParams[0]}
			err = matcher.Provision(ctx)
			return matcher, err
		},
	)
	if err != nil {
		return nil, err
	}
	envOpts := append(unnamedPattern.CompileOptions(), namedPattern.CompileOptions()...)
	prgOpts := append(unnamedPattern.ProgramOptions(), namedPattern.ProgramOptions()...)
	return NewMatcherCELLibrary(envOpts, prgOpts), nil
}

// CaddyModule returns the Caddy module information.
func (MatchProtocol) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.protocol",
		New: func() caddy.Module { return new(MatchProtocol) },
	}
}

// Match returns true if r matches m.
func (m MatchProtocol) Match(r *http.Request) bool {
	switch string(m) {
	case "grpc":
		return strings.HasPrefix(r.Header.Get("content-type"), "application/grpc")
	case "https":
		return r.TLS != nil
	case "http":
		return r.TLS == nil
	}
	return false
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchProtocol) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		var proto string
		if !d.Args(&proto) {
			return d.Err("expected exactly one protocol")
		}
		*m = MatchProtocol(proto)
	}
	return nil
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression protocol('https')
func (MatchProtocol) CELLibrary(_ caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		"protocol",
		"protocol_request_string",
		[]*exprpb.Type{decls.String},
		func(data ref.Val) (RequestMatcher, error) {
			protocolStr, ok := data.(types.String)
			if !ok {
				return nil, errors.New("protocol argument was not a string")
			}
			return MatchProtocol(strings.ToLower(string(protocolStr))), nil
		},
	)
}

// CaddyModule returns the Caddy module information.
func (MatchNot) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.not",
		New: func() caddy.Module { return new(MatchNot) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchNot) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		matcherSet, err := ParseCaddyfileNestedMatcherSet(d)
		if err != nil {
			return err
		}
		m.MatcherSetsRaw = append(m.MatcherSetsRaw, matcherSet)
	}
	return nil
}

// UnmarshalJSON satisfies json.Unmarshaler. It puts the JSON
// bytes directly into m's MatcherSetsRaw field.
func (m *MatchNot) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &m.MatcherSetsRaw)
}

// MarshalJSON satisfies json.Marshaler by marshaling
// m's raw matcher sets.
func (m MatchNot) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.MatcherSetsRaw)
}

// Provision loads the matcher modules to be negated.
func (m *MatchNot) Provision(ctx caddy.Context) error {
	matcherSets, err := ctx.LoadModule(m, "MatcherSetsRaw")
	if err != nil {
		return fmt.Errorf("loading matcher sets: %v", err)
	}
	for _, modMap := range matcherSets.([]map[string]interface{}) {
		var ms MatcherSet
		for _, modIface := range modMap {
			ms = append(ms, modIface.(RequestMatcher))
		}
		m.MatcherSets = append(m.MatcherSets, ms)
	}
	return nil
}

// Match returns true if r matches m. Since this matcher negates
// the embedded matchers, false is returned if any of its matcher
// sets return true.
func (m MatchNot) Match(r *http.Request) bool {
	for _, ms := range m.MatcherSets {
		if ms.Match(r) {
			return false
		}
	}
	return true
}

// CaddyModule returns the Caddy module information.
func (MatchRemoteIP) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.remote_ip",
		New: func() caddy.Module { return new(MatchRemoteIP) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *MatchRemoteIP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextArg() {
			if d.Val() == "forwarded" {
				if len(m.Ranges) > 0 {
					return d.Err("if used, 'forwarded' must be first argument")
				}
				m.Forwarded = true
				continue
			}
			if d.Val() == "private_ranges" {
				m.Ranges = append(m.Ranges, []string{
					"192.168.0.0/16",
					"172.16.0.0/12",
					"10.0.0.0/8",
					"127.0.0.1/8",
					"fd00::/8",
					"::1",
				}...)
				continue
			}
			m.Ranges = append(m.Ranges, d.Val())
		}
		if d.NextBlock(0) {
			return d.Err("malformed remote_ip matcher: blocks are not supported")
		}
	}
	return nil
}

// CELLibrary produces options that expose this matcher for use in CEL
// expression matchers.
//
// Example:
//    expression remote_ip('forwarded', '192.168.0.0/16', '172.16.0.0/12', '10.0.0.0/8')
func (MatchRemoteIP) CELLibrary(ctx caddy.Context) (cel.Library, error) {
	return CELMatcherImpl(
		// name of the macro, this is the function name that users see when writing expressions.
		"remote_ip",
		// name of the function that the macro will be rewritten to call.
		"remote_ip_match_request_list",
		// internal data type of the MatchPath value.
		[]*exprpb.Type{CelTypeListString},
		// function to convert a constant list of strings to a MatchPath instance.
		func(data ref.Val) (RequestMatcher, error) {
			refStringList := reflect.TypeOf([]string{})
			strList, err := data.ConvertToNative(refStringList)
			if err != nil {
				return nil, err
			}

			m := MatchRemoteIP{}

			for _, input := range strList.([]string) {
				if input == "forwarded" {
					if len(m.Ranges) > 0 {
						return nil, errors.New("if used, 'forwarded' must be first argument")
					}
					m.Forwarded = true
					continue
				}
				m.Ranges = append(m.Ranges, input)
			}

			err = m.Provision(ctx)
			return m, err
		},
	)
}

// Provision parses m's IP ranges, either from IP or CIDR expressions.
func (m *MatchRemoteIP) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)
	for _, str := range m.Ranges {
		// Exclude the zone_id from the IP
		if strings.Contains(str, "%") {
			split := strings.Split(str, "%")
			str = split[0]
			// write zone identifiers in m.zones for matching later
			m.zones = append(m.zones, split[1])
		} else {
			m.zones = append(m.zones, "")
		}
		if strings.Contains(str, "/") {
			_, ipNet, err := net.ParseCIDR(str)
			if err != nil {
				return fmt.Errorf("parsing CIDR expression '%s': %v", str, err)
			}
			m.cidrs = append(m.cidrs, ipNet)
		} else {
			ip := net.ParseIP(str)
			if ip == nil {
				return fmt.Errorf("invalid IP address: %s", str)
			}
			mask := len(ip) * 8
			m.cidrs = append(m.cidrs, &net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(mask, mask),
			})
		}
	}
	return nil
}

func (m MatchRemoteIP) getClientIP(r *http.Request) (net.IP, string, error) {
	remote := r.RemoteAddr
	zoneID := ""
	if m.Forwarded {
		if fwdFor := r.Header.Get("X-Forwarded-For"); fwdFor != "" {
			remote = strings.TrimSpace(strings.Split(fwdFor, ",")[0])
		}
	}
	ipStr, _, err := net.SplitHostPort(remote)
	if err != nil {
		ipStr = remote // OK; probably didn't have a port
	}
	// Some IPv6-Adresses can contain zone identifiers at the end,
	// which are separated with "%"
	if strings.Contains(ipStr, "%") {
		split := strings.Split(ipStr, "%")
		ipStr = split[0]
		zoneID = split[1]
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, zoneID, fmt.Errorf("invalid client IP address: %s", ipStr)
	}
	return ip, zoneID, nil
}

// Match returns true if r matches m.
func (m MatchRemoteIP) Match(r *http.Request) bool {
	clientIP, zoneID, err := m.getClientIP(r)
	if err != nil {
		m.logger.Error("getting client IP", zap.Error(err))
		return false
	}
	zoneFilter := true
	for i, ipRange := range m.cidrs {
		if ipRange.Contains(clientIP) {
			// Check if there are zone filters assigned and if they match.
			if m.zones[i] == "" || zoneID == m.zones[i] {
				return true
			}
			zoneFilter = false
		}
	}
	if !zoneFilter {
		m.logger.Debug("zone ID from remote did not match", zap.String("zone", zoneID))
	}
	return false
}

// MatchRegexp is an embedable type for matching
// using regular expressions. It adds placeholders
// to the request's replacer.
type MatchRegexp struct {
	// A unique name for this regular expression. Optional,
	// but useful to prevent overwriting captures from other
	// regexp matchers.
	Name string `json:"name,omitempty"`

	// The regular expression to evaluate, in RE2 syntax,
	// which is the same general syntax used by Go, Perl,
	// and Python. For details, see
	// [Go's regexp package](https://golang.org/pkg/regexp/).
	// Captures are accessible via placeholders. Unnamed
	// capture groups are exposed as their numeric, 1-based
	// index, while named capture groups are available by
	// the capture group name.
	Pattern string `json:"pattern"`

	compiled *regexp.Regexp
	phPrefix string
}

// Provision compiles the regular expression.
func (mre *MatchRegexp) Provision(caddy.Context) error {
	re, err := regexp.Compile(mre.Pattern)
	if err != nil {
		return fmt.Errorf("compiling matcher regexp %s: %v", mre.Pattern, err)
	}
	mre.compiled = re
	mre.phPrefix = regexpPlaceholderPrefix
	if mre.Name != "" {
		mre.phPrefix += "." + mre.Name
	}
	return nil
}

// Validate ensures mre is set up correctly.
func (mre *MatchRegexp) Validate() error {
	if mre.Name != "" && !wordRE.MatchString(mre.Name) {
		return fmt.Errorf("invalid regexp name (must contain only word characters): %s", mre.Name)
	}
	return nil
}

// Match returns true if input matches the compiled regular
// expression in mre. It sets values on the replacer repl
// associated with capture groups, using the given scope
// (namespace).
func (mre *MatchRegexp) Match(input string, repl *caddy.Replacer) bool {
	matches := mre.compiled.FindStringSubmatch(input)
	if matches == nil {
		return false
	}

	// save all capture groups, first by index
	for i, match := range matches {
		key := mre.phPrefix + "." + strconv.Itoa(i)
		repl.Set(key, match)
	}

	// then by name
	for i, name := range mre.compiled.SubexpNames() {
		if i != 0 && name != "" {
			key := mre.phPrefix + "." + name
			repl.Set(key, matches[i])
		}
	}

	return true
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (mre *MatchRegexp) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		args := d.RemainingArgs()
		switch len(args) {
		case 1:
			mre.Pattern = args[0]
		case 2:
			mre.Name = args[0]
			mre.Pattern = args[1]
		default:
			return d.ArgErr()
		}
		if d.NextBlock(0) {
			return d.Err("malformed path_regexp matcher: blocks are not supported")
		}
	}
	return nil
}

// ParseCaddyfileNestedMatcher parses the Caddyfile tokens for a nested
// matcher set, and returns its raw module map value.
func ParseCaddyfileNestedMatcherSet(d *caddyfile.Dispenser) (caddy.ModuleMap, error) {
	matcherMap := make(map[string]RequestMatcher)

	// in case there are multiple instances of the same matcher, concatenate
	// their tokens (we expect that UnmarshalCaddyfile should be able to
	// handle more than one segment); otherwise, we'd overwrite other
	// instances of the matcher in this set
	tokensByMatcherName := make(map[string][]caddyfile.Token)
	for nesting := d.Nesting(); d.NextArg() || d.NextBlock(nesting); {
		matcherName := d.Val()
		tokensByMatcherName[matcherName] = append(tokensByMatcherName[matcherName], d.NextSegment()...)
	}

	for matcherName, tokens := range tokensByMatcherName {
		mod, err := caddy.GetModule("http.matchers." + matcherName)
		if err != nil {
			return nil, d.Errf("getting matcher module '%s': %v", matcherName, err)
		}
		unm, ok := mod.New().(caddyfile.Unmarshaler)
		if !ok {
			return nil, d.Errf("matcher module '%s' is not a Caddyfile unmarshaler", matcherName)
		}
		err = unm.UnmarshalCaddyfile(caddyfile.NewDispenser(tokens))
		if err != nil {
			return nil, err
		}
		rm, ok := unm.(RequestMatcher)
		if !ok {
			return nil, fmt.Errorf("matcher module '%s' is not a request matcher", matcherName)
		}
		matcherMap[matcherName] = rm
	}

	// we should now have a functional matcher, but we also
	// need to be able to marshal as JSON, otherwise config
	// adaptation will be missing the matchers!
	matcherSet := make(caddy.ModuleMap)
	for name, matcher := range matcherMap {
		jsonBytes, err := json.Marshal(matcher)
		if err != nil {
			return nil, fmt.Errorf("marshaling %T matcher: %v", matcher, err)
		}
		matcherSet[name] = jsonBytes
	}

	return matcherSet, nil
}

var (
	wordRE = regexp.MustCompile(`\w+`)
)

const regexpPlaceholderPrefix = "http.regexp"

// MatcherErrorVarKey is the key used for the variable that
// holds an optional error emitted from a request matcher,
// to short-circuit the handler chain, since matchers cannot
// return errors via the RequestMatcher interface.
const MatcherErrorVarKey = "matchers.error"

// Interface guards
var (
	_ RequestMatcher    = (*MatchHost)(nil)
	_ caddy.Provisioner = (*MatchHost)(nil)
	_ RequestMatcher    = (*MatchPath)(nil)
	_ RequestMatcher    = (*MatchPathRE)(nil)
	_ caddy.Provisioner = (*MatchPathRE)(nil)
	_ RequestMatcher    = (*MatchMethod)(nil)
	_ RequestMatcher    = (*MatchQuery)(nil)
	_ RequestMatcher    = (*MatchHeader)(nil)
	_ RequestMatcher    = (*MatchHeaderRE)(nil)
	_ caddy.Provisioner = (*MatchHeaderRE)(nil)
	_ RequestMatcher    = (*MatchProtocol)(nil)
	_ RequestMatcher    = (*MatchRemoteIP)(nil)
	_ caddy.Provisioner = (*MatchRemoteIP)(nil)
	_ RequestMatcher    = (*MatchNot)(nil)
	_ caddy.Provisioner = (*MatchNot)(nil)
	_ caddy.Provisioner = (*MatchRegexp)(nil)

	_ caddyfile.Unmarshaler = (*MatchHost)(nil)
	_ caddyfile.Unmarshaler = (*MatchPath)(nil)
	_ caddyfile.Unmarshaler = (*MatchPathRE)(nil)
	_ caddyfile.Unmarshaler = (*MatchMethod)(nil)
	_ caddyfile.Unmarshaler = (*MatchQuery)(nil)
	_ caddyfile.Unmarshaler = (*MatchHeader)(nil)
	_ caddyfile.Unmarshaler = (*MatchHeaderRE)(nil)
	_ caddyfile.Unmarshaler = (*MatchProtocol)(nil)
	_ caddyfile.Unmarshaler = (*MatchRemoteIP)(nil)
	_ caddyfile.Unmarshaler = (*VarsMatcher)(nil)
	_ caddyfile.Unmarshaler = (*MatchVarsRE)(nil)

	_ CELLibraryProducer = (*MatchHost)(nil)
	_ CELLibraryProducer = (*MatchPath)(nil)
	_ CELLibraryProducer = (*MatchPathRE)(nil)
	_ CELLibraryProducer = (*MatchMethod)(nil)
	_ CELLibraryProducer = (*MatchQuery)(nil)
	_ CELLibraryProducer = (*MatchHeader)(nil)
	_ CELLibraryProducer = (*MatchHeaderRE)(nil)
	_ CELLibraryProducer = (*MatchProtocol)(nil)
	_ CELLibraryProducer = (*MatchRemoteIP)(nil)
	// _ CELLibraryProducer = (*VarsMatcher)(nil)
	// _ CELLibraryProducer = (*MatchVarsRE)(nil)

	_ json.Marshaler   = (*MatchNot)(nil)
	_ json.Unmarshaler = (*MatchNot)(nil)
)
