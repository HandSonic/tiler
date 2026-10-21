package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/paulmach/orb/maptile"
)

const defaultTilePath = "{z}/{x}/{y}.{format}"

type tilePathPart struct {
	literal string
	token   string
}

type tilePathTemplate struct {
	pattern string
	parts   []tilePathPart
	tokens  []string
	matcher *regexp.Regexp
}

// Patterns come from task configuration, not individual tile requests. Sharing
// immutable compiled templates avoids rebuilding the parser for every tile.
var tilePathTemplates sync.Map

func normalizeTilePath(pattern string) (string, error) {
	template, err := compiledTilePath(pattern)
	if err != nil {
		return "", err
	}
	return template.pattern, nil
}

func compiledTilePath(pattern string) (*tilePathTemplate, error) {
	if pattern == "" {
		pattern = defaultTilePath
	}
	if cached, ok := tilePathTemplates.Load(pattern); ok {
		return cached.(*tilePathTemplate), nil
	}
	template := &tilePathTemplate{pattern: pattern}
	var expression strings.Builder
	expression.WriteByte('^')
	found := make(map[string]bool)
	previousNumber, separated := false, false
	for cursor := 0; cursor < len(pattern); {
		switch pattern[cursor] {
		case '{':
			end := strings.IndexByte(pattern[cursor+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("tile path contains an unclosed placeholder")
			}
			end += cursor + 1
			token := pattern[cursor+1 : end]
			switch token {
			case "z", "x", "y", "-y":
				if previousNumber && !separated {
					return nil, fmt.Errorf("tile path numeric placeholders require a non-digit separator to avoid coordinate collisions")
				}
				previousNumber, separated = true, false
				expression.WriteString("([^/]+?)")
			case "format":
				separated = true
				expression.WriteString("((?i:png|jpg|jpeg|pbf|webp))")
			default:
				return nil, fmt.Errorf("unsupported tile path placeholder {%s}", token)
			}
			found[token] = true
			template.parts = append(template.parts, tilePathPart{token: token})
			template.tokens = append(template.tokens, token)
			cursor = end + 1
		case '}':
			return nil, fmt.Errorf("tile path contains an unmatched closing brace")
		default:
			end := cursor
			for end < len(pattern) && pattern[end] != '{' && pattern[end] != '}' {
				if pattern[end] < '0' || pattern[end] > '9' {
					separated = true
				}
				end++
			}
			literal := pattern[cursor:end]
			template.parts = append(template.parts, tilePathPart{literal: literal})
			expression.WriteString(regexp.QuoteMeta(literal))
			cursor = end
		}
	}
	if !found["z"] || !found["x"] || found["y"] == found["-y"] {
		return nil, fmt.Errorf("tile path must contain {z}, {x} and exactly one of {y} or {-y}")
	}
	// Sampling zero and one catches dynamic reserved Windows names such as
	// COM{x}; maximum coordinate widths catch filenames that would become too
	// long later in the task. Every emitted path is checked again after rendering.
	for _, sample := range [][4]string{{"0", "0", "0", "0"}, {"1", "1", "1", "1"}, {"30", "1073741823", "1073741823", "1073741823"}} {
		if err := validateRelativeTilePath(template.render(sample[0], sample[1], sample[2], sample[3], "webp")); err != nil {
			return nil, err
		}
	}
	expression.WriteByte('$')
	matcher, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("compile tile path: %w", err)
	}
	template.matcher = matcher
	actual, _ := tilePathTemplates.LoadOrStore(pattern, template)
	return actual.(*tilePathTemplate), nil
}

func (template *tilePathTemplate) render(z, x, y, invertedY, format string) string {
	var result strings.Builder
	for _, part := range template.parts {
		switch part.token {
		case "z":
			result.WriteString(z)
		case "x":
			result.WriteString(x)
		case "y":
			result.WriteString(y)
		case "-y":
			result.WriteString(invertedY)
		case "format":
			result.WriteString(format)
		default:
			result.WriteString(part.literal)
		}
	}
	return result.String()
}

func renderTilePath(pattern string, tile maptile.Tile, format string) (string, error) {
	template, err := compiledTilePath(pattern)
	if err != nil {
		return "", err
	}
	if err := validateFailureCoordinates(int(tile.Z), tile.X, tile.Y); err != nil {
		return "", fmt.Errorf("render tile path: %w", err)
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if err := validateFailureFormat(format); err != nil {
		return "", fmt.Errorf("render tile path: %w", err)
	}
	result := template.render(
		strconv.FormatUint(uint64(tile.Z), 10),
		strconv.FormatUint(uint64(tile.X), 10),
		strconv.FormatUint(uint64(tile.Y), 10),
		strconv.FormatUint((uint64(1)<<uint(tile.Z))-1-uint64(tile.Y), 10), format)
	if err := validateRelativeTilePath(result); err != nil {
		return "", err
	}
	return result, nil
}

// parseTilePath parses slash-separated relative names from an XYZ directory.
// A static name or unsupported extension that does not fit the template is
// unrelated input; matching names with invalid coordinates are reported.
func parseTilePath(pattern, relative string) (tile maptile.Tile, format string, matched bool, err error) {
	template, err := compiledTilePath(pattern)
	if err != nil {
		return tile, "", false, err
	}
	match := template.matcher.FindStringSubmatch(relative)
	if match == nil {
		return tile, "", false, nil
	}
	if err := validateRelativeTilePath(relative); err != nil {
		return tile, "", true, err
	}
	coordinates := make(map[string]uint64)
	for index, token := range template.tokens {
		value := match[index+1]
		if token == "format" {
			value = strings.ToLower(value)
			if format != "" && format != value {
				return tile, "", true, fmt.Errorf("tile path has inconsistent repeated {format} values")
			}
			format = value
			continue
		}
		number, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return tile, "", true, fmt.Errorf("tile path {%s} coordinate must be a nonnegative integer", token)
		}
		if previous, ok := coordinates[token]; ok && previous != number {
			return tile, "", true, fmt.Errorf("tile path has inconsistent repeated {%s} coordinates", token)
		}
		coordinates[token] = number
	}
	z, x := coordinates["z"], coordinates["x"]
	y, isXYZ := coordinates["y"]
	if z > 30 {
		return tile, "", true, fmt.Errorf("tile path zoom must be between 0 and 30")
	}
	if !isXYZ {
		limit := uint64(1) << uint(z)
		if coordinates["-y"] >= limit {
			return tile, "", true, fmt.Errorf("tile path {-y} coordinate is outside the zoom grid")
		}
		y = limit - 1 - coordinates["-y"]
	}
	tile = maptile.Tile{Z: maptile.Zoom(z), X: uint32(x), Y: uint32(y)}
	if err := validateFailureCoordinates(int(z), tile.X, tile.Y); err != nil {
		return maptile.Tile{}, "", true, fmt.Errorf("parse tile path: %w", err)
	}
	return tile, format, true, nil
}

// validateTilePathFileConflicts rejects templates that could use a reserved
// local file as a directory, replace that file, or create a tile where one of
// its parent directories is needed. Numeric captures deliberately accept any
// number here: checking a conservative path shape avoids enumerating tiles.
// An empty format checks every supported encoding, before format inference.
func validateTilePathFileConflicts(pattern, format string, reservedRelativePaths ...string) error {
	template, err := compiledTilePath(pattern)
	if err != nil {
		return err
	}
	format = strings.ToLower(strings.TrimSpace(format))
	formatExpression := "(?:png|jpg|jpeg|pbf|webp)"
	if format != "" {
		if err := validateFailureFormat(format); err != nil {
			return fmt.Errorf("check tile path conflicts: %w", err)
		}
		formatExpression = regexp.QuoteMeta(format)
	}
	segments := strings.Split(template.pattern, "/")
	matchers := make([]*regexp.Regexp, len(segments))
	replacer := strings.NewReplacer(
		regexp.QuoteMeta("{z}"), "[0-9]+",
		regexp.QuoteMeta("{x}"), "[0-9]+",
		regexp.QuoteMeta("{y}"), "[0-9]+",
		regexp.QuoteMeta("{-y}"), "[0-9]+",
		regexp.QuoteMeta("{format}"), formatExpression,
	)
	for index, segment := range segments {
		matcher, err := regexp.Compile("(?i)^" + replacer.Replace(regexp.QuoteMeta(segment)) + "$")
		if err != nil {
			return fmt.Errorf("compile tile path conflict check: %w", err)
		}
		matchers[index] = matcher
	}
	for _, relative := range reservedRelativePaths {
		if err := validateRelativeTilePath(relative); err != nil {
			return fmt.Errorf("reserved local file %q: %w", relative, err)
		}
		reserved := strings.Split(relative, "/")
		common := len(reserved)
		if common > len(matchers) {
			common = len(matchers)
		}
		conflict := true
		for index := 0; index < common; index++ {
			if !matchers[index].MatchString(reserved[index]) {
				conflict = false
				break
			}
		}
		if conflict {
			return fmt.Errorf("tile path %q may conflict with reserved local file %q", template.pattern, relative)
		}
	}
	return nil
}

func validateRelativeTilePath(relative string) error {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") || !utf8.ValidString(relative) {
		return fmt.Errorf("tile path must be a relative slash-separated path")
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("tile path cannot contain empty, . or .. segments")
		}
		if strings.TrimRight(segment, " .") != segment || len(utf16.Encode([]rune(segment))) > 255 {
			return fmt.Errorf("tile path segments must fit Windows filenames without trailing dots or spaces")
		}
		if strings.ContainsAny(segment, `<>:"|?*`) || strings.IndexFunc(segment, unicode.IsControl) >= 0 {
			return fmt.Errorf("tile path contains characters that are invalid in Windows filenames")
		}
		base := strings.ToUpper(strings.TrimRight(strings.SplitN(segment, ".", 2)[0], " "))
		reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL"
		for _, prefix := range []string{"COM", "LPT"} {
			if strings.HasPrefix(base, prefix) {
				suffix := strings.TrimPrefix(base, prefix)
				reserved = reserved || (len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9') || suffix == "¹" || suffix == "²" || suffix == "³"
			}
		}
		if reserved {
			return fmt.Errorf("tile path contains reserved Windows filename %q", segment)
		}
	}
	return nil
}
