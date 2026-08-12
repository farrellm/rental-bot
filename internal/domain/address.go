package domain

import (
	"slices"
	"strings"
)

// NormalizeAddress folds an address into the canonical form stored in
// properties.normalized_address and matched against in the ingestion pipeline.
//
// Property matching is deterministic Go, never the LLM's job (docs/DESIGN.md
// §5.3): the model returns an address string, this function folds it, and the
// result is compared against the column. Both sides of that comparison go
// through here, so the only property that matters is that the folding is
// total and stable — a rule that is imperfect USPS but applied identically to
// the stored address and the extracted one still matches them to each other.
//
// Three transforms, in order:
//
//   - Unit designators are stripped. "412 Elm St Apt 2" and "412 Elm St"
//     normalize the same, because the unit lives in the units table and an
//     invoice addressed to a unit is still an invoice about the building.
//   - USPS-style abbreviations are folded, so STREET and ST agree.
//   - Case and punctuation collapse to single-spaced uppercase.
//
// The result is stable across calls but is not a display address; it is a
// match key, and nothing should ever render it to a user.
func NormalizeAddress(line1, line2, city, state, postal string) string {
	street := foldStreet(tokenize(line1 + " " + line2))

	parts := make([]string, 0, 4)
	if len(street) > 0 {
		parts = append(parts, strings.Join(street, " "))
	}
	if c := strings.Join(tokenize(city), " "); c != "" {
		parts = append(parts, c)
	}
	if s := foldState(tokenize(state)); s != "" {
		parts = append(parts, s)
	}
	if z := foldPostal(postal); z != "" {
		parts = append(parts, z)
	}
	return strings.Join(parts, " ")
}

// FreeAddress is a single-string address broken into the fields
// NormalizeAddress takes.
//
// A model returns one string, because that is how an address appears on a
// document. This is where it becomes the five fields the stored side already
// keeps it in, so both halves of a comparison go through the same folding.
type FreeAddress struct {
	Line1  string
	City   string
	State  string
	Postal string
}

// ParseFreeAddress splits an address string on its commas.
//
// Commas are what a printed address uses to separate the street from the town
// and the town from the state, and reading them is the whole of the rule. A
// string with no commas is all street: that fold still matches on Street
// below, which is what the matcher falls back to and why guessing where the
// street ends is not worth doing.
//
// The last part is checked for a state and a ZIP because "OH 45701" is one
// comma-separated field on every envelope.
func ParseFreeAddress(s string) FreeAddress {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	parts = slices.DeleteFunc(parts, func(p string) bool { return p == "" })

	var a FreeAddress
	if len(parts) == 0 {
		return a
	}
	a.Line1 = parts[0]
	if len(parts) == 1 {
		return a
	}

	// The tail carries the state and the ZIP; everything between it and the
	// street is the town, which can legitimately be more than one part.
	a.State, a.Postal = splitRegion(parts[len(parts)-1])
	rest := parts[1 : len(parts)-1]
	if a.State == "" && a.Postal == "" {
		// The last part was neither, so it is part of the town after all.
		rest = parts[1:]
	}
	a.City = strings.Join(rest, " ")
	return a
}

// splitRegion reads "OH 45701", "Ohio", or "45701" off the end of an address.
func splitRegion(s string) (state, postal string) {
	tokens := tokenize(s)
	if len(tokens) > 0 && isPostal(tokens[len(tokens)-1]) {
		postal = tokens[len(tokens)-1]
		tokens = tokens[:len(tokens)-1]
	}
	joined := strings.Join(tokens, " ")
	if joined == "" {
		return "", postal
	}
	if _, named := states[joined]; named || (len(joined) == 2 && isStateCode(joined)) {
		return joined, postal
	}
	// Neither a state nor a ZIP: the caller treats it as more town.
	if postal == "" {
		return "", ""
	}
	return "", postal
}

// isPostal reports whether a token is a five- or nine-digit ZIP.
func isPostal(t string) bool {
	if len(t) != 5 && len(t) != 9 {
		return false
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isStateCode reports whether a two-letter token is one of the postal codes.
func isStateCode(t string) bool {
	for _, code := range states {
		if code == t {
			return true
		}
	}
	return false
}

// Key is the full fold, to compare against properties.normalized_address.
func (a FreeAddress) Key() string {
	return NormalizeAddress(a.Line1, "", a.City, a.State, a.Postal)
}

// Street is the street line alone, folded.
//
// It is what a comparison falls back to when one side names a town and the
// other does not — an invoice that says only "412 Elm St" is still an invoice
// about the building at 412 Elm St, and a receipt for a house in the next
// county is not going to share a house number and a street name with one of
// yours.
func (a FreeAddress) Street() string {
	return NormalizeAddress(a.Line1, "", "", "", "")
}

// tokenize uppercases, reduces every non-alphanumeric byte to a separator, and
// splits. '#' survives as its own token because it introduces a unit number
// and dropUnits needs to see it; everything else about it is punctuation.
func tokenize(s string) []string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '#':
			b.WriteString(" # ")
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// foldStreet applies the street-line rules: drop the unit, fold directionals
// wherever they appear, and fold the suffix where a suffix can appear.
func foldStreet(tokens []string) []string {
	tokens = dropUnits(tokens)

	for i, t := range tokens {
		if d, ok := directionals[t]; ok {
			tokens[i] = d
		}
	}

	// The suffix map only applies in the suffix position. Folding it
	// everywhere would rewrite "3 Mill Rd" to "3 ML RD", because MILL is a
	// USPS suffix as well as an ordinary street name.
	if i := suffixIndex(tokens); i >= 0 {
		if s, ok := suffixes[tokens[i]]; ok {
			tokens[i] = s
		}
	}
	return tokens
}

// suffixIndex returns the position a street suffix would occupy, or -1.
//
// Normally that is the last token. An address can carry a post-directional
// after its suffix — "412 Elm Street NW" — so a trailing directional is
// stepped over.
func suffixIndex(tokens []string) int {
	i := len(tokens) - 1
	if i < 0 {
		return -1
	}
	if _, isDir := directionals[tokens[i]]; isDir && i > 0 {
		i--
	}
	return i
}

// dropUnits removes each unit designator and the token that identifies the
// unit, so that the fold describes a building rather than a door.
//
// The filter is in place, so this overwrites its argument. Every caller passes
// a slice tokenize has just built and nobody else holds, which is what makes
// that safe; the index loop is deliberate too, because a designator consumes
// the token after it.
func dropUnits(tokens []string) []string {
	out := tokens[:0]
	for i := 0; i < len(tokens); i++ {
		if _, ok := unitDesignators[tokens[i]]; ok {
			i++ // and the unit number with it
			continue
		}
		out = append(out, tokens[i])
	}
	return out
}

// foldState maps a spelled-out state to its two-letter code. A document may
// say "Ohio" where the form said "OH".
func foldState(tokens []string) string {
	joined := strings.Join(tokens, " ")
	if code, ok := states[joined]; ok {
		return code
	}
	return joined
}

// foldPostal reduces ZIP+4 to the five-digit code. The extra four are a
// delivery route, not part of the address's identity.
func foldPostal(s string) string {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	z := digits.String()
	if len(z) > 5 {
		z = z[:5]
	}
	return z
}

// unitDesignators introduce a unit, floor, or box that distinguishes a door
// within a building. The set is USPS Publication 28 Appendix C2 plus '#'.
var unitDesignators = map[string]struct{}{
	"APARTMENT": {}, "APT": {}, "BASEMENT": {}, "BSMT": {}, "BLDG": {},
	"BUILDING": {}, "DEPARTMENT": {}, "DEPT": {}, "FL": {}, "FLOOR": {},
	"FRNT": {}, "HANGAR": {}, "HNGR": {}, "KEY": {}, "LBBY": {}, "LOBBY": {},
	"LOT": {}, "LOWER": {}, "LOWR": {}, "OFC": {}, "OFFICE": {}, "PH": {},
	"PIER": {}, "REAR": {}, "RM": {}, "ROOM": {}, "SIDE": {}, "SLIP": {},
	"SPACE": {}, "SPC": {}, "STE": {}, "STOP": {}, "SUITE": {}, "TRAILER": {},
	"TRLR": {}, "UNIT": {}, "UPPER": {}, "UPPR": {}, "#": {},
}

// directionals fold wherever they appear, because they can precede or follow
// the street name.
var directionals = map[string]string{
	"NORTH": "N", "SOUTH": "S", "EAST": "E", "WEST": "W",
	"NORTHEAST": "NE", "NORTHWEST": "NW",
	"SOUTHEAST": "SE", "SOUTHWEST": "SW",
	"N": "N", "S": "S", "E": "E", "W": "W",
	"NE": "NE", "NW": "NW", "SE": "SE", "SW": "SW",
}

// suffixes maps the common spellings of a street suffix onto the USPS standard
// abbreviation. This is the residential subset of Publication 28 Appendix C1 —
// enough to cover the addresses this application will ever hold, and each entry
// maps its own abbreviation to itself so folding is idempotent.
var suffixes = map[string]string{
	"ALLEY": "ALY", "ALY": "ALY",
	"AVENUE": "AVE", "AV": "AVE", "AVE": "AVE", "AVEN": "AVE", "AVENU": "AVE",
	"BEND": "BND", "BND": "BND",
	"BOULEVARD": "BLVD", "BLVD": "BLVD", "BOUL": "BLVD", "BOULV": "BLVD",
	"BRANCH": "BR", "BR": "BR",
	"BRIDGE": "BRG", "BRG": "BRG",
	"BROOK": "BRK", "BRK": "BRK",
	"BYPASS": "BYP", "BYP": "BYP",
	"CANYON": "CYN", "CYN": "CYN",
	"CENTER": "CTR", "CTR": "CTR", "CENTRE": "CTR",
	"CIRCLE": "CIR", "CIR": "CIR", "CIRC": "CIR", "CIRCL": "CIR",
	"CLIFF": "CLF", "CLF": "CLF",
	"COMMONS": "CMNS", "CMNS": "CMNS",
	"CORNER": "COR", "COR": "COR",
	"COURT": "CT", "CT": "CT", "CRT": "CT",
	"COVE": "CV", "CV": "CV",
	"CREEK": "CRK", "CRK": "CRK",
	"CRESCENT": "CRES", "CRES": "CRES",
	"CROSSING": "XING", "XING": "XING", "CRSSNG": "XING",
	"DALE": "DL", "DL": "DL",
	"DRIVE": "DR", "DR": "DR", "DRIV": "DR", "DRV": "DR",
	"ESTATE": "EST", "EST": "EST",
	"ESTATES": "ESTS", "ESTS": "ESTS",
	"EXPRESSWAY": "EXPY", "EXPY": "EXPY", "EXPRESS": "EXPY", "EXPW": "EXPY",
	"EXTENSION": "EXT", "EXT": "EXT", "EXTN": "EXT",
	"FALLS": "FLS", "FLS": "FLS",
	"FIELD": "FLD", "FLD": "FLD",
	"FIELDS": "FLDS", "FLDS": "FLDS",
	"FORD": "FRD", "FRD": "FRD",
	"FOREST": "FRST", "FRST": "FRST",
	"FORK": "FRK", "FRK": "FRK",
	"FREEWAY": "FWY", "FWY": "FWY", "FRWAY": "FWY", "FRWY": "FWY",
	"GARDEN": "GDN", "GDN": "GDN", "GARDN": "GDN", "GRDN": "GDN",
	"GARDENS": "GDNS", "GDNS": "GDNS",
	"GLEN": "GLN", "GLN": "GLN",
	"GREEN": "GRN", "GRN": "GRN",
	"GROVE": "GRV", "GRV": "GRV", "GROV": "GRV",
	"HARBOR": "HBR", "HBR": "HBR", "HARB": "HBR", "HARBR": "HBR",
	"HEIGHTS": "HTS", "HTS": "HTS", "HT": "HTS",
	"HIGHWAY": "HWY", "HWY": "HWY", "HIWAY": "HWY", "HIWY": "HWY", "HWAY": "HWY",
	"HILL": "HL", "HL": "HL",
	"HILLS": "HLS", "HLS": "HLS",
	"HOLLOW": "HOLW", "HOLW": "HOLW", "HLLW": "HOLW",
	"ISLAND": "IS", "IS": "IS",
	"JUNCTION": "JCT", "JCT": "JCT", "JCTN": "JCT",
	"KNOLL": "KNL", "KNL": "KNL", "KNOL": "KNL",
	"LAKE": "LK", "LK": "LK",
	"LAKES": "LKS", "LKS": "LKS",
	"LANDING": "LNDG", "LNDG": "LNDG",
	"LANE": "LN", "LN": "LN",
	"LOOP":  "LOOP",
	"MANOR": "MNR", "MNR": "MNR",
	"MEADOW": "MDW", "MDW": "MDW",
	"MEADOWS": "MDWS", "MDWS": "MDWS",
	"MILL": "ML", "ML": "ML",
	"MILLS": "MLS", "MLS": "MLS",
	"MOUNT": "MT", "MT": "MT", "MNT": "MT",
	"MOUNTAIN": "MTN", "MTN": "MTN", "MNTN": "MTN",
	"ORCHARD": "ORCH", "ORCH": "ORCH", "ORCHRD": "ORCH",
	"PARK":    "PARK",
	"PARKWAY": "PKWY", "PKWY": "PKWY", "PARKWY": "PKWY", "PKWAY": "PKWY", "PKY": "PKWY",
	"PASS": "PASS",
	"PATH": "PATH",
	"PIKE": "PIKE",
	"PINE": "PNE", "PNE": "PNE",
	"PINES": "PNES", "PNES": "PNES",
	"PLACE": "PL", "PL": "PL",
	"PLAIN": "PLN", "PLN": "PLN",
	"PLAZA": "PLZ", "PLZ": "PLZ", "PLZA": "PLZ",
	"POINT": "PT", "PT": "PT",
	"POINTE":  "PT",
	"PRAIRIE": "PR", "PR": "PR",
	"RIDGE": "RDG", "RDG": "RDG", "RDGE": "RDG",
	"RIVER": "RIV", "RIV": "RIV", "RVR": "RIV", "RIVR": "RIV",
	"ROAD": "RD", "RD": "RD",
	"ROUTE": "RTE", "RTE": "RTE",
	"RUN":   "RUN",
	"SHOAL": "SHL", "SHL": "SHL",
	"SHORE": "SHR", "SHR": "SHR", "SHOAR": "SHR",
	"SPRING": "SPG", "SPG": "SPG", "SPNG": "SPG", "SPRNG": "SPG",
	"SPRINGS": "SPGS", "SPGS": "SPGS", "SPNGS": "SPGS",
	"SQUARE": "SQ", "SQ": "SQ", "SQR": "SQ", "SQU": "SQ",
	"STATION": "STA", "STA": "STA", "STATN": "STA", "STN": "STA",
	"STREET": "ST", "ST": "ST", "STR": "ST", "STRT": "ST",
	"SUMMIT": "SMT", "SMT": "SMT", "SUMIT": "SMT",
	"TERRACE": "TER", "TER": "TER", "TERR": "TER",
	"TRACE": "TRCE", "TRCE": "TRCE",
	"TRAIL": "TRL", "TRL": "TRL", "TRAILS": "TRL",
	"TURNPIKE": "TPKE", "TPKE": "TPKE", "TRNPK": "TPKE", "TURNPK": "TPKE",
	"UNION": "UN", "UN": "UN",
	"VALLEY": "VLY", "VLY": "VLY", "VALLY": "VLY", "VLLY": "VLY",
	"VIEW": "VW", "VW": "VW",
	"VILLAGE": "VLG", "VLG": "VLG", "VILL": "VLG", "VILLAG": "VLG", "VILLG": "VLG",
	"VISTA": "VIS", "VIS": "VIS",
	"WALK": "WALK",
	"WAY":  "WAY", "WY": "WAY",
	"WELLS": "WLS", "WLS": "WLS",
}

// states maps a spelled-out state or territory to its postal code.
var states = map[string]string{
	"ALABAMA": "AL", "ALASKA": "AK", "ARIZONA": "AZ", "ARKANSAS": "AR",
	"CALIFORNIA": "CA", "COLORADO": "CO", "CONNECTICUT": "CT", "DELAWARE": "DE",
	"DISTRICT OF COLUMBIA": "DC", "FLORIDA": "FL", "GEORGIA": "GA",
	"HAWAII": "HI", "IDAHO": "ID", "ILLINOIS": "IL", "INDIANA": "IN",
	"IOWA": "IA", "KANSAS": "KS", "KENTUCKY": "KY", "LOUISIANA": "LA",
	"MAINE": "ME", "MARYLAND": "MD", "MASSACHUSETTS": "MA", "MICHIGAN": "MI",
	"MINNESOTA": "MN", "MISSISSIPPI": "MS", "MISSOURI": "MO", "MONTANA": "MT",
	"NEBRASKA": "NE", "NEVADA": "NV", "NEW HAMPSHIRE": "NH", "NEW JERSEY": "NJ",
	"NEW MEXICO": "NM", "NEW YORK": "NY", "NORTH CAROLINA": "NC",
	"NORTH DAKOTA": "ND", "OHIO": "OH", "OKLAHOMA": "OK", "OREGON": "OR",
	"PENNSYLVANIA": "PA", "PUERTO RICO": "PR", "RHODE ISLAND": "RI",
	"SOUTH CAROLINA": "SC", "SOUTH DAKOTA": "SD", "TENNESSEE": "TN",
	"TEXAS": "TX", "UTAH": "UT", "VERMONT": "VT", "VIRGINIA": "VA",
	"WASHINGTON": "WA", "WEST VIRGINIA": "WV", "WISCONSIN": "WI",
	"WYOMING": "WY",
}
