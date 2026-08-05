package domain

import "testing"

func TestNormalizeAddress(t *testing.T) {
	tests := []struct {
		name                              string
		line1, line2, city, state, postal string
		want                              string
	}{
		{
			name:  "spelled-out suffix folds",
			line1: "412 Elm Street", city: "Athens", state: "OH", postal: "45701",
			want: "412 ELM ST ATHENS OH 45701",
		},
		{
			name:  "punctuation and case collapse",
			line1: "412  elm st.", city: "athens,", state: "oh", postal: "45701",
			want: "412 ELM ST ATHENS OH 45701",
		},
		{
			name:  "a unit designator and its number are stripped",
			line1: "412 Elm St", line2: "Apt 2", city: "Athens", state: "OH", postal: "45701",
			want: "412 ELM ST ATHENS OH 45701",
		},
		{
			name:  "a hash unit is stripped whether or not it is spaced",
			line1: "412 Elm St #2B", city: "Athens", state: "OH", postal: "45701",
			want: "412 ELM ST ATHENS OH 45701",
		},
		{
			name:  "a pre-directional folds",
			line1: "88 North Oak Avenue", city: "Athens", state: "OH", postal: "45701",
			want: "88 N OAK AVE ATHENS OH 45701",
		},
		{
			name:  "a suffix before a post-directional still folds",
			line1: "88 Oak Avenue Northwest", city: "Athens", state: "OH", postal: "45701",
			want: "88 OAK AVE NW ATHENS OH 45701",
		},
		{
			name:  "a spelled-out state folds to its code",
			line1: "3 Mill Rd", city: "Athens", state: "Ohio", postal: "45701",
			want: "3 MILL RD ATHENS OH 45701",
		},
		{
			name:  "zip+4 reduces to the five-digit code",
			line1: "3 Mill Rd", city: "Athens", state: "OH", postal: "45701-2233",
			want: "3 MILL RD ATHENS OH 45701",
		},
		{
			name:  "a two-word state folds",
			line1: "10 Pine St", city: "Concord", state: "New Hampshire", postal: "03301",
			want: "10 PINE ST CONCORD NH 03301",
		},
		{
			name:  "missing fields simply drop out",
			line1: "412 Elm St",
			want:  "412 ELM ST",
		},
		{
			name: "an empty address normalizes to empty",
			want: "",
		},
	}

	for _, tt := range tests {
		got := NormalizeAddress(tt.line1, tt.line2, tt.city, tt.state, tt.postal)
		if got != tt.want {
			t.Errorf("%s: NormalizeAddress = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestNormalizeAddressFoldsSuffixOnlyInTheSuffixPosition(t *testing.T) {
	// MILL, PARK, and UNION are USPS suffixes and also ordinary street names.
	// Folding them wherever they appeared would rewrite the street name itself
	// and collide two different streets onto one key.
	tests := []struct {
		line1 string
		want  string
	}{
		{"3 Mill Creek Road", "3 MILL CREEK RD"},
		{"3 Mill Road", "3 MILL RD"},
		{"90 Park Place", "90 PARK PL"},
		{"1 Union Street", "1 UNION ST"},
	}
	for _, tt := range tests {
		if got := NormalizeAddress(tt.line1, "", "", "", ""); got != tt.want {
			t.Errorf("NormalizeAddress(%q) = %q, want %q", tt.line1, got, tt.want)
		}
	}
}

func TestNormalizeAddressAgreesAcrossSpellings(t *testing.T) {
	// The property this function exists for: the address a form captured and
	// the address a model read off a document have to land on the same key.
	// Neither spelling is canonical; only their agreement matters.
	spellings := [][5]string{
		{"412 Elm Street", "Apt 2", "Athens", "Ohio", "45701-2233"},
		{"412 ELM ST.", "#2", "ATHENS", "OH", "45701"},
		{"412 elm st", "Unit 2", "Athens,", "oh", "45701"},
		{"412 Elm St, Apartment 2", "", "Athens", "Ohio", "45701"},
	}

	want := NormalizeAddress(spellings[0][0], spellings[0][1], spellings[0][2], spellings[0][3], spellings[0][4])
	for _, s := range spellings[1:] {
		if got := NormalizeAddress(s[0], s[1], s[2], s[3], s[4]); got != want {
			t.Errorf("NormalizeAddress%v = %q, want %q", s, got, want)
		}
	}
}

func TestNormalizeAddressIsIdempotent(t *testing.T) {
	// The stored column is fed back through matching, so folding an already
	// folded address must not change it again.
	for _, line1 := range []string{
		"412 Elm Street", "88 North Oak Avenue Southwest", "3 Mill Rd", "1 Union St",
	} {
		once := NormalizeAddress(line1, "", "Athens", "OH", "45701")
		twice := NormalizeAddress(once, "", "", "", "")
		if once != twice {
			t.Errorf("NormalizeAddress(%q) = %q, folding again gave %q", line1, once, twice)
		}
	}
}
