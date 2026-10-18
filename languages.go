package main

import (
	"fmt"
	"regexp"
)

// languageCodePattern matches the codes used for subtitles: "en", "tr", "pt-br", "eng".
var languageCodePattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})?$`)

var languageNames = map[string]string{
	"ar": "Arabic", "bg": "Bulgarian", "cs": "Czech", "da": "Danish", "de": "German", "el": "Greek", "en": "English",
	"es": "Spanish", "fa": "Persian", "fi": "Finnish", "fr": "French", "he": "Hebrew", "hi": "Hindi", "hr": "Croatian",
	"hu": "Hungarian", "id": "Indonesian", "it": "Italian", "ja": "Japanese", "ko": "Korean", "nl": "Dutch",
	"no": "Norwegian", "pl": "Polish", "pt": "Portuguese", "pt-br": "Brazilian Portuguese", "ro": "Romanian",
	"ru": "Russian", "sk": "Slovak", "sr": "Serbian", "sv": "Swedish", "th": "Thai", "tr": "Turkish",
	"uk": "Ukrainian", "vi": "Vietnamese", "zh": "Chinese", "zh-cn": "Simplified Chinese", "zh-tw": "Traditional Chinese",
}

// languageName is what the translation prompt calls a language; unknown codes are described by their code.
func languageName(code string) string {
	if name, found := languageNames[code]; found {
		return name
	}
	return fmt.Sprintf("the language with the code %q", code)
}
