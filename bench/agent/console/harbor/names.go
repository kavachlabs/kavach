package harbor

import (
	"math/rand/v2"
	"path/filepath"
)

// Job names are adjective-colour-animal and a random suffix
// (brisk-cobalt-heron-x7k2p9q): easier to tell apart and remember than a
// timestamp. The jobs list sorts by start time, not by name.
var (
	nameAdjectives = []string{
		"agile", "amber", "ancient", "bold", "brave", "breezy", "bright", "brisk",
		"calm", "candid", "clever", "cosmic", "crisp", "curious", "daring", "dapper",
		"eager", "early", "fabled", "fancy", "fearless", "fleet", "gentle", "giddy",
		"glad", "grand", "hardy", "hidden", "humble", "jolly", "keen", "kind",
		"lively", "lucky", "merry", "mighty", "nimble", "noble", "patient", "plucky",
		"polite", "proud", "quick", "quiet", "rapid", "restless", "rustic", "sharp",
		"shy", "silent", "sly", "snappy", "steady", "stoic", "sturdy", "sunny",
		"swift", "tidy", "tranquil", "vivid", "wandering", "wild", "wise", "witty",
	}
	nameColours = []string{
		"amber", "aqua", "azure", "bronze", "cedar", "cherry", "cobalt", "copper",
		"coral", "crimson", "ember", "golden", "granite", "hazel", "indigo", "ivory",
		"jade", "lemon", "lilac", "maroon", "mint", "ochre", "olive", "onyx",
		"pearl", "plum", "rose", "ruby", "rust", "saffron", "sage", "scarlet",
		"silver", "slate", "teal", "umber",
	}
	nameAnimals = []string{
		"badger", "beaver", "bison", "bobcat", "condor", "coyote", "crane", "dingo",
		"dolphin", "eagle", "falcon", "ferret", "finch", "fox", "gecko", "gibbon",
		"heron", "ibex", "jackal", "jaguar", "kestrel", "koala", "lemur", "lynx",
		"magpie", "marmot", "marten", "mole", "moose", "narwhal", "newt", "ocelot",
		"orca", "osprey", "otter", "owl", "panda", "panther", "pelican", "puffin",
		"quail", "raven", "robin", "salmon", "seal", "shrike", "sparrow", "stoat",
		"swan", "tapir", "tern", "tiger", "toucan", "viper", "walrus", "weasel",
		"wolf", "wombat", "wren", "yak",
	}
)

const suffixChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomName is three words and seven letters and digits, the last as in
// Harbor's attempt ids: 36^7 (78 billion) suffixes for each of the words.
func randomName() string {
	adj := nameAdjectives[rand.IntN(len(nameAdjectives))]
	colour := nameColours[rand.IntN(len(nameColours))]
	for colour == adj { // "amber-amber-…"
		colour = nameColours[rand.IntN(len(nameColours))]
	}
	suffix := make([]byte, 7)
	for i := range suffix {
		suffix[i] = suffixChars[rand.IntN(len(suffixChars))]
	}
	return adj + "-" + colour + "-" + nameAnimals[rand.IntN(len(nameAnimals))] + "-" + string(suffix)
}

// newName picks a name no job has taken yet.
func (r *Runner) newName() string {
	for {
		n := randomName()
		if !exists(filepath.Join(r.JobsDir, n)) && !exists(r.file(n, ".json")) {
			return n
		}
	}
}
