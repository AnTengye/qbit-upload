package cmd

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// catalogNumber is the structured result of preprocessing a release name.
// Code is the stable value used for deduplication and reporting. Part and Tags
// describe filename metadata that must not become part of Code.
type catalogNumber struct {
	Raw         string
	Code        string
	Part        int
	Tags        []string
	Pattern     string
	MatchedFrom string
}

type catalogCandidate struct {
	code      string
	codeStart int
	codeEnd   int
	score     int
	pattern   string
}

var (
	domainPattern = regexp.MustCompile(`(?i)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|top|xyz|xxx|tv|cc|me|app|vip|club|site|info|biz|io|co|cn|jp|ru|la|pw|pro|live|online|store|work|link|ink|one|fun|icu|lol)\b`)

	fc2Pattern       = regexp.MustCompile(`(?:^|[^A-Z0-9])FC2(?:[-_. ]*PPV)?[-_. ]*([0-9]{5,9})(?:[^0-9]|$)`)
	heydougaPattern  = regexp.MustCompile(`(?:^|[^A-Z0-9])(?:HEYDOUGA|HEY)[-_. ]*(?:HD[-_. ]*)?([0-9]{4})[-_. ]+0?([0-9]{3,5})(?:[^0-9]|$)`)
	heyzoPattern     = regexp.MustCompile(`(?:^|[^A-Z0-9])HEYZO[-_. ]*(?:HD|LT)?[-_. ]*([0-9]{3,6})(?:[^0-9]|$)`)
	platformPattern  = regexp.MustCompile(`(?:^|[^A-Z0-9])(GETCHU|GYUTTO|GCOLLE|PCOLLE|MYWIFE)[-_ ]*([0-9]{3,9})(?:[^0-9]|$)`)
	dmmCIDPattern    = regexp.MustCompile(`(?:^|[^A-Z0-9])((?:H_[0-9]{3,4}[A-Z]{1,10}[0-9]{2,5}[A-Z0-9]{0,8})|(?:[0-9]{3}_[0-9]{4,5})|(?:402[A-Z]{3,6}[0-9]*_[A-Z]{3,8}[0-9]{5,6})|(?:[0-9]{3,4}WVR[0-9][A-Z0-9][0-9]{4,5}[A-Z0-9]{0,8}))(?:[^A-Z0-9]|$)`)
	mugenPattern     = regexp.MustCompile(`(?:^|[^A-Z0-9])((?:(?:MKD|MKBD)[-_ ]*S[0-9]{2,3})|(?:(?:MK3D2DBD|S2M|S2MBD)[-_ ]*[0-9]{2,3}))(?:[^A-Z0-9]|$)`)
	dateMakerPattern = regexp.MustCompile(`(?:^|[^A-Z0-9])(?:CARIB(?:BEAN|EAN)?(?:COM)?(?:PR)?|1?POND(?:O|P)?|10MU(?:SUME)?|PACO(?:PACO)?(?:MAMA)?|MURA(?:MURA)?)[-_ ]*([0-9]{6})[-_]([0-9]{2,3})(?:[^0-9]|$)`)
	extendedDateRE   = regexp.MustCompile(`(?:^|[^A-Z0-9])(DL1PON|1PONDP)[-_ ]*([0-9]{6})[-_]([0-9]{2,3})(?:[^0-9]|$)`)
	uncensoredMaker  = regexp.MustCompile(`(?:^|[^A-Z0-9])(H4610|H0930|C0930)[-_ ]*([A-Z]{1,4}[0-9]{2,8})(?:[^A-Z0-9]|$)`)
	xxxAVPattern     = regexp.MustCompile(`(?:^|[^A-Z0-9])XXX[-_ ]*AV[-_ ]*([0-9]{2,8})(?:[^0-9]|$)`)
	alphaSerialRE    = regexp.MustCompile(`(?:^|[^A-Z0-9])([A-Z]{2,10})[-_ ]+([A-Z])([0-9]{2,6})([AB]?)(HHB[0-9]*|CH|UC|C|U)?(?:[^A-Z0-9]|$)`)
	digitStandardRE  = regexp.MustCompile(`(?:^|[^A-Z0-9])([0-9]{1,4}[A-Z][A-Z0-9]{1,10})[-_ ]+([0-9]{2,8})([EZ]?)([AB]?)(HHB[0-9]*|CH|UC|C|U)?(?:[^A-Z0-9]|$)`)
	specialPrefixRE  = regexp.MustCompile(`(?:^|[^A-Z0-9])(T28|T38|R18)[-_ ]*([0-9]{3,8})([EZ]?)([AB]?)(HHB[0-9]*|CH|UC|C|U)?(?:[^A-Z0-9]|$)`)
	standardPattern  = regexp.MustCompile(`(?:^|[^A-Z0-9])([A-Z][A-Z0-9]{1,11})[-_ ]+([0-9]{2,8})([EZ]?)([AB]?)(HHB[0-9]*|CH|UC|C|U)?(?:[^A-Z0-9]|$)`)
	compactPattern   = regexp.MustCompile(`(?:^|[^A-Z0-9])([A-Z]{2,10})([0-9]{2,8})([EZ]?)([AB]?)(HHB[0-9]*|CH|UC|C|U)?(?:[^A-Z0-9]|$)`)
	digitCompactRE   = regexp.MustCompile(`(?:^|[^A-Z0-9])([0-9]{1,4}[A-Z]{2,10}[0-9]{2,8})(HHB[0-9]*)?(?:[^A-Z0-9]|$)`)
	knownCompactRE   = regexp.MustCompile(`(?:^|[^A-Z0-9])(RED[01][0-9]{2}|SKY[0-3][0-9]{2}|EX00[01][0-9]|(?:CZ|GEDO|KB|SE)[0-9]{2,4})(?:[^A-Z0-9]|$)`)
	tDashPattern     = regexp.MustCompile(`(?:^|[^A-Z0-9])(T)[-_ ]([0-9]{5})(?:[^0-9]|$)`)
	singlePrefixRE   = regexp.MustCompile(`(?:^|[^A-Z0-9])([NK])[-_ ]?([0-9]{4,7})(?:[^A-Z0-9]|$)`)
	dateCatalogRE    = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})[-_]([0-9]{2,3})(?:[^0-9]|$)`)
	partNumberRE     = regexp.MustCompile(`^[._ -]+(?:(?:CD|PT|PART|DISC|DISK)[._ -]*)?([0-9]{1,2})(?:[^0-9]|$)`)
	partLetterRE     = regexp.MustCompile(`^[._ -]*([AB])(?:[^A-Z0-9]|$)`)
	timestampTagRE   = regexp.MustCompile(`(?:^|[^0-9])[0-9]{8}[_-][0-9]{6}(?:[^0-9]|$)`)
	quality4KTagRE   = regexp.MustCompile(`(?:^|[^A-Z0-9])(?:4K|2160P)(?:[^A-Z0-9]|$)`)
	quality1080TagRE = regexp.MustCompile(`(?:^|[^A-Z0-9])1080P(?:[^A-Z0-9]|$)`)
	quality720TagRE  = regexp.MustCompile(`(?:^|[^A-Z0-9])720P(?:[^A-Z0-9]|$)`)
)

var catalogNoisePrefixes = map[string]struct{}{
	"AVC": {}, "DISC": {}, "DISK": {}, "FHD": {}, "H264": {}, "H265": {},
	"HD": {}, "HEVC": {}, "HHD": {}, "MOVIE": {}, "PART": {}, "SAMPLE": {},
	"SD": {}, "THUMBNAIL": {}, "TRAILER": {}, "UHD": {}, "VIDEO": {}, "VOL": {},
	"VOLUME": {}, "X264": {}, "X265": {},
}

// preprocessCatalogNumber extracts a canonical catalog number from a file or
// archive path. The closest parent directory is used only when the basename
// contains no valid candidate.
func preprocessCatalogNumber(name string) (catalogNumber, bool) {
	cleanedPath := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	cleanedPath = strings.Trim(cleanedPath, "/")
	if cleanedPath == "" {
		return catalogNumber{}, false
	}

	components := strings.Split(cleanedPath, "/")
	for i := len(components) - 1; i >= 0; i-- {
		component := components[i]
		if i == len(components)-1 {
			component = strings.TrimSuffix(component, path.Ext(component))
		}
		result, ok := preprocessCatalogComponent(component)
		if !ok {
			continue
		}
		result.Raw = name
		result.MatchedFrom = components[i]
		return result, true
	}
	return catalogNumber{}, false
}

func preprocessCatalogComponent(component string) (catalogNumber, bool) {
	value := strings.ToUpper(strings.TrimSpace(component))
	if value == "" {
		return catalogNumber{}, false
	}
	value = maskDomains(strings.ReplaceAll(value, ")(", "-"))

	candidates := findCatalogCandidates(value)
	if len(candidates) == 0 {
		return catalogNumber{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].codeStart < candidates[j].codeStart
	})

	selected := candidates[0]
	result := catalogNumber{
		Code:    selected.code,
		Pattern: selected.pattern,
	}
	if selected.codeEnd < len(value) {
		result.Part, result.Tags = parseCatalogSuffix(value[selected.codeEnd:])
	}
	return result, true
}

func maskDomains(value string) string {
	masked := []byte(value)
	for _, bounds := range domainPattern.FindAllStringIndex(value, -1) {
		for i := bounds[0]; i < bounds[1]; i++ {
			masked[i] = ' '
		}
	}
	return string(masked)
}

func findCatalogCandidates(value string) []catalogCandidate {
	var candidates []catalogCandidate

	for _, match := range fc2Pattern.FindAllStringSubmatchIndex(value, -1) {
		id := capture(value, match, 1)
		candidates = append(candidates, catalogCandidate{
			code: "FC2-" + id, codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1),
			score: 220, pattern: "fc2",
		})
	}
	// A malformed FC2 label must not fall through and turn an adjacent title
	// fragment such as "WRONG 12345" into a normal catalog number.
	if strings.Contains(value, "FC2") {
		return candidates
	}
	for _, match := range heydougaPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      "HEYDOUGA-" + capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 215, pattern: "heydouga",
		})
	}
	for _, match := range heyzoPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code: "HEYZO-" + capture(value, match, 1), codeStart: captureStart(match, 1),
			codeEnd: captureEnd(match, 1), score: 210, pattern: "heyzo",
		})
	}
	for _, match := range platformPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 205, pattern: "platform",
		})
	}
	for _, match := range dmmCIDPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code: capture(value, match, 1), codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1),
			score: 200, pattern: "dmm-cid",
		})
	}
	for _, match := range mugenPattern.FindAllStringSubmatchIndex(value, -1) {
		code := strings.NewReplacer("_", "-", " ", "-").Replace(capture(value, match, 1))
		candidates = append(candidates, catalogCandidate{
			code: code, codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1), score: 195, pattern: "mugen",
		})
	}
	for _, match := range dateMakerPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 192, pattern: "date-maker",
		})
	}
	for _, match := range extendedDateRE.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2) + "-" + capture(value, match, 3),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 3), score: 192, pattern: "extended-date",
		})
	}
	for _, match := range uncensoredMaker.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 190, pattern: "uncensored-maker",
		})
	}
	for _, match := range xxxAVPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      "XXX-AV-" + capture(value, match, 1),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1), score: 190, pattern: "xxx-av",
		})
	}
	for _, match := range alphaSerialRE.FindAllStringSubmatchIndex(value, -1) {
		prefix := capture(value, match, 1)
		if _, noisy := catalogNoisePrefixes[prefix]; noisy {
			continue
		}
		candidates = append(candidates, catalogCandidate{
			code:      prefix + "-" + capture(value, match, 2) + capture(value, match, 3),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 3), score: 188, pattern: "alpha-serial",
		})
	}
	addStandardCandidates(&candidates, value, digitStandardRE, 180, "digit-leading-standard")
	addStandardCandidates(&candidates, value, specialPrefixRE, 185, "special-prefix")
	addStandardCandidates(&candidates, value, standardPattern, 175, "standard")
	for _, match := range knownCompactRE.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code: capture(value, match, 1), codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1),
			score: 165, pattern: "known-compact",
		})
	}
	for _, match := range digitCompactRE.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code: capture(value, match, 1), codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 1),
			score: 160, pattern: "digit-leading-compact",
		})
	}
	addStandardCandidates(&candidates, value, compactPattern, 145, "compact")

	for _, match := range singlePrefixRE.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 140, pattern: "single-prefix",
		})
	}
	for _, match := range tDashPattern.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 138, pattern: "t-dash",
		})
	}
	for _, match := range dateCatalogRE.FindAllStringSubmatchIndex(value, -1) {
		candidates = append(candidates, catalogCandidate{
			code:      capture(value, match, 1) + "-" + capture(value, match, 2),
			codeStart: captureStart(match, 1), codeEnd: captureEnd(match, 2), score: 120, pattern: "date",
		})
	}
	return candidates
}

func addStandardCandidates(candidates *[]catalogCandidate, value string, re *regexp.Regexp, score int, pattern string) {
	for _, match := range re.FindAllStringSubmatchIndex(value, -1) {
		prefix := capture(value, match, 1)
		if _, noisy := catalogNoisePrefixes[prefix]; noisy {
			continue
		}
		digits := capture(value, match, 2)
		semanticSuffix := capture(value, match, 3)
		*candidates = append(*candidates, catalogCandidate{
			code:      prefix + "-" + digits + semanticSuffix,
			codeStart: captureStart(match, 1),
			codeEnd:   captureEnd(match, 3),
			score:     score + min(len(prefix), 10),
			pattern:   pattern,
		})
	}
}

func parseCatalogSuffix(suffix string) (int, []string) {
	upper := strings.ToUpper(suffix)
	part := 0
	if match := partNumberRE.FindStringSubmatch(upper); len(match) == 2 {
		if parsed, err := strconv.Atoi(match[1]); err == nil && parsed > 0 {
			part = parsed
		}
	} else if match := partLetterRE.FindStringSubmatch(upper); len(match) == 2 {
		part = int(match[1][0]-'A') + 1
	}

	var tags []string
	if timestampTagRE.MatchString(upper) {
		tags = append(tags, "timestamp")
	}
	if quality4KTagRE.MatchString(upper) {
		tags = append(tags, "4k")
	} else if quality1080TagRE.MatchString(upper) {
		tags = append(tags, "1080p")
	} else if quality720TagRE.MatchString(upper) {
		tags = append(tags, "720p")
	}
	if hasCatalogToken(upper, "CH") || hasCatalogToken(upper, "C") || hasCatalogToken(upper, "UC") {
		tags = append(tags, "chinese-subtitles")
	}
	if hasCatalogToken(upper, "U") || hasCatalogToken(upper, "UC") {
		tags = append(tags, "uncensored")
	}
	return part, tags
}

func hasCatalogToken(value, token string) bool {
	for _, field := range strings.FieldsFunc(value, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
	}) {
		if field == token {
			return true
		}
	}
	return value == token
}

func capture(value string, match []int, group int) string {
	start, end := captureStart(match, group), captureEnd(match, group)
	if start < 0 || end < 0 {
		return ""
	}
	return value[start:end]
}

func captureStart(match []int, group int) int {
	index := group * 2
	if index >= len(match) {
		return -1
	}
	return match[index]
}

func captureEnd(match []int, group int) int {
	index := group*2 + 1
	if index >= len(match) || match[index] < 0 {
		// Optional semantic suffixes should leave Code ending at the preceding
		// numeric group.
		if group > 1 {
			return captureEnd(match, group-1)
		}
		return -1
	}
	return match[index]
}
