/** @typedef {"en" | "tr"} Language */

export const LANGUAGES = /** @type {const} */ (["en", "tr"]);

/** @type {Record<Language, Record<string, string>>} */
export const messages = {
    en: {
        "nav.home": "subbed home",
        "hero.title": "YouTube with your own subtitles",
        "hero.hint": "Paste a YouTube link. If the library has subtitles for it, they appear on top of the video.",
        "url.label": "YouTube link",
        "url.placeholder": "https://youtube.com/watch?v=…",
        "url.play": "Play",
        "status.loading": "Loading…",
        "error.invalid": "That doesn't look like a YouTube link.",
        "error.network": "Can't reach the server.",
        "error.server": "Couldn't load the video (error {status}).",
        "error.retry": "Try again",
        "error.home": "Start over",
        "notice.noSubtitles": "This video has no subtitles in the library. YouTube's own captions are used when available.",
        "subtitles.label": "Subtitles",
        "subtitles.off": "Off",
        "size.label": "Size",
        "size.smaller": "Smaller subtitles",
        "size.larger": "Larger subtitles",
        "delay.label": "Delay",
        "delay.earlier": "Show subtitles earlier",
        "delay.later": "Show subtitles later",
        "delay.reset": "Reset delay",
        "fullscreen.enter": "Fullscreen",
        "fullscreen.exit": "Exit fullscreen",
        "settings.title": "Settings",
        "settings.language": "Language",
        "settings.languageAuto": "Automatic",
        "settings.theme": "Appearance",
        "theme.system": "System",
        "theme.light": "Light",
        "theme.dark": "Dark",
    },
    tr: {
        "nav.home": "subbed ana sayfa",
        "hero.title": "Kendi altyazılarınla YouTube",
        "hero.hint": "Bir YouTube bağlantısı yapıştır. Kütüphanede altyazısı varsa videonun üzerinde görünür.",
        "url.label": "YouTube bağlantısı",
        "url.placeholder": "https://youtube.com/watch?v=…",
        "url.play": "Oynat",
        "status.loading": "Yükleniyor…",
        "error.invalid": "Bu bir YouTube bağlantısına benzemiyor.",
        "error.network": "Sunucuya ulaşılamıyor.",
        "error.server": "Video yüklenemedi (hata {status}).",
        "error.retry": "Tekrar dene",
        "error.home": "Baştan başla",
        "notice.noSubtitles": "Bu videonun kütüphanede altyazısı yok. Varsa YouTube'un kendi altyazıları kullanılır.",
        "subtitles.label": "Altyazı",
        "subtitles.off": "Kapalı",
        "size.label": "Boyut",
        "size.smaller": "Altyazıyı küçült",
        "size.larger": "Altyazıyı büyüt",
        "delay.label": "Gecikme",
        "delay.earlier": "Altyazıyı erken göster",
        "delay.later": "Altyazıyı geç göster",
        "delay.reset": "Gecikmeyi sıfırla",
        "fullscreen.enter": "Tam ekran",
        "fullscreen.exit": "Tam ekrandan çık",
        "settings.title": "Ayarlar",
        "settings.language": "Dil",
        "settings.languageAuto": "Otomatik",
        "settings.theme": "Görünüm",
        "theme.system": "Sistem",
        "theme.light": "Açık",
        "theme.dark": "Koyu",
    },
};

/**
 * Pick the UI language from a saved preference, falling back to the browser languages, then English.
 * @param {string | null} preference "en", "tr" or anything else for automatic
 * @param {readonly string[]} browserLanguages e.g. navigator.languages
 * @returns {Language}
 */
export function resolveLanguage(preference, browserLanguages) {
    if (LANGUAGES.includes(/** @type {Language} */ (preference))) return /** @type {Language} */ (preference);
    for (const tag of browserLanguages) {
        const base = tag.toLowerCase().split("-")[0];
        if (LANGUAGES.includes(/** @type {Language} */ (base))) return /** @type {Language} */ (base);
    }
    return "en";
}

/**
 * Look up a message and fill `{placeholders}`; falls back to English, then to the key.
 * @param {Language} language
 * @param {string} key
 * @param {Record<string, string | number>} [params]
 * @returns {string}
 */
export function translate(language, key, params = {}) {
    const template = messages[language][key] ?? messages.en[key] ?? key;
    return template.replace(/\{(\w+)\}/g, (placeholder, name) => String(params[name] ?? placeholder));
}
