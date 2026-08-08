package main

import (
	"log"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isSpammyContent checks for various forms of spammy or undesirable content.
func isSpammyContent(s string) bool {
	// 1. Check for excessive length.
	runeCount := utf8.RuneCountInString(s)
	if runeCount > 2000 {
		log.Printf("[FILTER] Message blocked due to excessive length: %d runes", runeCount)
		return true
	}

	// 2. Check for repeated character spam (e.g., "######...").
	var lastRune rune
	repeatCount := 0
	for _, r := range s {
		if r == lastRune && !unicode.IsSpace(r) {
			repeatCount++
		} else {
			repeatCount = 1
			lastRune = r
		}
		if repeatCount > 20 { // More than 20 consecutive non-space characters is spam.
			log.Printf("[FILTER] Message blocked due to repeated character spam ('%c')", r)
			return true
		}
	}

	// 3. Check character category ratio.
	if runeCount == 0 {
		return false
	}
	allowedChars := 0
	for _, r := range s {
		if isAllowedRune(r) {
			allowedChars++
		}
	}
	allowedRatio := float64(allowedChars) / float64(runeCount)

	// If a message is mostly made of non-standard characters, it's spam.
	// This is the main check that will catch kaomoji and symbol spam.
	if allowedRatio < 0.80 {
		log.Printf("[FILTER] Message blocked due to low allowed character ratio: %.2f", allowedRatio)
		return true
	}

	return false
}

// isAllowedRune defines the set of characters considered "normal" for conversation.
// This is intentionally strict to exclude most kaomoji/symbol spam.
func isAllowedRune(r rune) bool {
	return unicode.In(r, unicode.Latin) || // Restrict to Latin-based scripts
		unicode.IsDigit(r) ||
		unicode.IsSpace(r) ||
		isEmoji(r) ||
		strings.ContainsRune(`.,'?!@#$%^&*()_+-=[]{}|\\;:"/<>~:`, r) // Common punctuation for chat
}

// isEmoji checks if a rune falls within common Unicode emoji ranges.
func isEmoji(r rune) bool {
	// A simplified check for common emoji ranges.
	return (r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
		(r >= 0x1F300 && r <= 0x1F5FF) || // Misc Symbols and Pictographs
		(r >= 0x1F680 && r <= 0x1F6FF) || // Transport and Map
		(r >= 0x2600 && r <= 0x26FF) || // Misc symbols
		(r >= 0x2700 && r <= 0x27BF) || // Dingbats
		(r >= 0xFE00 && r <= 0xFE0F) || // Variation Selectors
		(r >= 0x1F900 && r <= 0x1F9FF) || // Supplemental Symbols and Pictographs
		(r >= 0x1F1E6 && r <= 0x1F1FF) // Regional Indicator Symbols
}

// filterIllegalContent scans for keywords related to strictly prohibited topics.
// If a match is found, it logs the category and returns true to block the message entirely.
func filterIllegalContent(content string) bool {
	// NOTE: The user must populate these patterns with specific, explicit keywords.
	// The examples below are representative and use non-explicit terms to demonstrate functionality.
	patterns := map[string]string{
		// Category: Violent Threats
		`\b(threaten|harm|attack|kill)\s+(someone|people|a group)\b`: "Violent Threat",
		// Category: Illegal Activities (using non-dangerous placeholders)
		`\b(how to build|create|make)\s+(a|an)\s+(dangerous item|weapon)\b`: "Illegal Activities",
		// CSAM-related keywords would be added here by the user.
	}

	for pattern, category := range patterns {
		re := regexp.MustCompile(`(?i)` + pattern)
		if re.MatchString(content) {
			log.Printf("[FILTER] Message BLOCKED due to sensitive content category: %s", category)
			return true // Block the message
		}
	}
	return false // Message is clean
}

// filterSlurs replaces specific slurs with harmless alternatives using case-insensitive, whole-word matching.
func filterSlurs(content string) string {
	replacements := map[string]string{
		`\bn[i1]gg(a|er|ar)s?\b`: "ninja",       // Replaces the racial slur for African Americans
		`\bf[a@]gg?[o0]ts?\b`:    "magician",    // Homophobic slur
		`\btrann(y|ie)s?\b`:      "transformer", // Transphobic slur
		`\bk[i1]ke\b`:            "wizard",      // Anti-Semitic slur
		`\bsp[i1]c\b`:            "spark",       // Racial slur for Hispanics/Latinos
		`\bch[i1]nk\b`:           "chime",       // Racial slur for East Asians
		`\bredsk[i1]n\b`:         "redwood",     // Racial slur for Native Americans
		`\bwetb[a@]ck\b`:         "waveback",    // Slur for Mexican immigrants
		`\bkaffir\b`:             "kangaroo",    // Severe slur for Black Africans
		`\bgook\b`:               "goofy",       // Slur for East Asians (war context)
		`\bpaki\b`:               "pinky",       // Slur for Pakistanis/South Asians
		`\bboche\b`:              "bouncer",     // Slur for Germans
		`\bcoolie\b`:             "cookie",      // Slur for South Asian/Chinese laborers
		`\bc[o0]{2}n\b`:          "doomer",      // Racial slur for Black people
		`\bsambo\b`:              "samba",       // Stereotypical slur for Black people
		`\byid\b`:                "yard",        // Anti-Semitic slur (Yiddish-related)
		`\bheeb\b`:               "healer",      // Anti-Semitic slur
		`\bwop\b`:                "whopper",     // Slur for Italians
		`\bmick\b`:               "mickey",      // Slur for Irish
		`\bchug\b`:               "chugger",     // Slur for Native Americans
		`\bdago\b`:               "dango",       // Slur for Italians
		`\bhonky\b`:              "honey",       // Slur for White people
		`\bcrack(a|er)\b`:        "cruncher",    // Slur for White people
	}

	for pattern, replacement := range replacements {
		// Prepending (?i) makes the regex case-insensitive.
		re := regexp.MustCompile(`(?i)` + pattern)
		content = re.ReplaceAllString(content, replacement)
	}
	return content
}

// containsSlur checks if a string contains any of the slurs defined in filterSlurs.
func containsSlur(content string) bool {
	return filterSlurs(content) != content
}
