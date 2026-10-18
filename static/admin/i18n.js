/** @typedef {"en" | "tr"} Language */

export const LANGUAGES = /** @type {const} */ (["en", "tr"]);

/** @type {Record<Language, Record<string, string>>} */
export const messages = {
    en: {
        "app.admin": "Admin",
        "nav.player": "Open player",
        "nav.logo": "Subbed admin home",
        "library.title": "Library",
        "library.search": "Search videos",
        "library.empty": "No videos yet",
        "library.emptyHint": "Add a YouTube video, then attach subtitles to it.",
        "library.noMatch": "Nothing matches “{query}”",
        "library.loadFailed": "Couldn't load the library",
        "library.retry": "Try again",
        "videos.one": "{n} video",
        "videos.other": "{n} videos",
        "subtitles.one": "{n} subtitle",
        "subtitles.other": "{n} subtitles",
        "video.add": "Add video",
        "video.addTitle": "Add a video",
        "video.url": "YouTube URL or video ID",
        "video.urlPlaceholder": "https://youtube.com/watch?v=…",
        "video.urlInvalid": "That doesn't look like a YouTube link",
        "video.name": "Title",
        "video.namePlaceholder": "Shown in the library",
        "video.titleFetching": "Fetching the title…",
        "video.noSubtitles": "No subtitles yet",
        "video.watch": "Play with subtitles",
        "video.delete": "Delete",
        "video.deleteLabel": "Delete video",
        "video.added": "Video added",
        "video.deleted": "Video deleted",
        "video.edit": "Edit",
        "video.editLabel": "Edit video title",
        "video.editTitle": "Edit video",
        "video.updated": "Video updated",
        "subtitle.upload": "Upload subtitle",
        "subtitle.add": "Add subtitle",
        "subtitle.video": "Video",
        "subtitle.forVideo": "Adding a subtitle to",
        "subtitle.pickVideo": "Choose a video",
        "subtitle.language": "Language code",
        "subtitle.languageHint": "Two or three letters, e.g. en or tr",
        "subtitle.languageInvalid": "Use a code like en, tr or pt-br",
        "subtitle.file": "Subtitle file",
        "subtitle.dropTitle": "Drop, paste or browse",
        "subtitle.dropHint": ".srt or .vtt — you can also drop a file anywhere on this page or paste it with Ctrl+V",
        "subtitle.dropNow": "Drop to upload",
        "subtitle.replace": "Replace",
        "subtitle.remove": "Remove file",
        "subtitle.pasted": "Pasted subtitles",
        "subtitle.uploaded": "Subtitle uploaded",
        "subtitle.deleted": "Subtitle deleted",
        "subtitle.delete": "Delete {language} subtitle",
        "subtitle.badFile": "Only .srt and .vtt files are supported",
        "subtitle.needVideo": "Add a video first",
        "subtitle.fileReady": "File ready — pick a video and a language",
        "common.cancel": "Cancel",
        "common.delete": "Delete",
        "common.save": "Save",
        "common.close": "Close",
        "common.working": "Working…",
        "confirm.video": "Delete “{label}” and all of its subtitles?",
        "confirm.subtitle": "Delete the {label} subtitle?",
        "confirm.title": "Are you sure?",
        "confirm.undo": "This can't be undone.",
        "settings.title": "Settings",
        "settings.language": "Language",
        "settings.languageAuto": "Automatic",
        "settings.theme": "Appearance",
        "theme.system": "System",
        "theme.light": "Light",
        "theme.dark": "Dark",
        "settings.install": "Install app",
        "settings.installHint": "Add Subbed admin to your device for quick access.",
        "settings.installManual": "Use your browser's menu and choose “Install” or “Add to Home Screen”.",
        "settings.installed": "Running as an installed app",
        "status.offline": "You're offline. The app opens, but the library and uploads need a connection.",
        "status.update": "A new version is ready.",
        "status.reload": "Reload",
        "error.network": "Can't reach the server",
        "error.http": "Request failed ({status})",
    },
    tr: {
        "app.admin": "Yönetim",
        "nav.player": "Oynatıcıyı aç",
        "nav.logo": "Subbed yönetim ana sayfası",
        "library.title": "Kütüphane",
        "library.search": "Video ara",
        "library.empty": "Henüz video yok",
        "library.emptyHint": "Bir YouTube videosu ekle, sonra altyazı bağla.",
        "library.noMatch": "“{query}” ile eşleşen yok",
        "library.loadFailed": "Kütüphane yüklenemedi",
        "library.retry": "Tekrar dene",
        "videos.one": "{n} video",
        "videos.other": "{n} video",
        "subtitles.one": "{n} altyazı",
        "subtitles.other": "{n} altyazı",
        "video.add": "Video ekle",
        "video.addTitle": "Video ekle",
        "video.url": "YouTube bağlantısı veya video kimliği",
        "video.urlPlaceholder": "https://youtube.com/watch?v=…",
        "video.urlInvalid": "Bu bir YouTube bağlantısına benzemiyor",
        "video.name": "Başlık",
        "video.namePlaceholder": "Kütüphanede görünür",
        "video.titleFetching": "Başlık getiriliyor…",
        "video.noSubtitles": "Henüz altyazı yok",
        "video.watch": "Altyazılı oynat",
        "video.delete": "Sil",
        "video.deleteLabel": "Videoyu sil",
        "video.added": "Video eklendi",
        "video.deleted": "Video silindi",
        "video.edit": "Düzenle",
        "video.editLabel": "Video başlığını düzenle",
        "video.editTitle": "Videoyu düzenle",
        "video.updated": "Video güncellendi",
        "subtitle.upload": "Altyazı yükle",
        "subtitle.add": "Altyazı ekle",
        "subtitle.video": "Video",
        "subtitle.forVideo": "Altyazı eklenen video",
        "subtitle.pickVideo": "Bir video seç",
        "subtitle.language": "Dil kodu",
        "subtitle.languageHint": "İki veya üç harf, örn. en veya tr",
        "subtitle.languageInvalid": "en, tr veya pt-br gibi bir kod kullan",
        "subtitle.file": "Altyazı dosyası",
        "subtitle.dropTitle": "Bırak, yapıştır veya gözat",
        "subtitle.dropHint": ".srt veya .vtt — dosyayı sayfanın herhangi bir yerine bırakabilir veya Ctrl+V ile yapıştırabilirsin",
        "subtitle.dropNow": "Yüklemek için bırak",
        "subtitle.replace": "Değiştir",
        "subtitle.remove": "Dosyayı kaldır",
        "subtitle.pasted": "Yapıştırılan altyazı",
        "subtitle.uploaded": "Altyazı yüklendi",
        "subtitle.deleted": "Altyazı silindi",
        "subtitle.delete": "{language} altyazısını sil",
        "subtitle.badFile": "Yalnızca .srt ve .vtt dosyaları desteklenir",
        "subtitle.needVideo": "Önce bir video ekle",
        "subtitle.fileReady": "Dosya hazır — bir video ve dil seç",
        "common.cancel": "Vazgeç",
        "common.delete": "Sil",
        "common.save": "Kaydet",
        "common.close": "Kapat",
        "common.working": "Çalışıyor…",
        "confirm.video": "“{label}” ve tüm altyazıları silinsin mi?",
        "confirm.subtitle": "{label} altyazısı silinsin mi?",
        "confirm.title": "Emin misin?",
        "confirm.undo": "Bu işlem geri alınamaz.",
        "settings.title": "Ayarlar",
        "settings.language": "Dil",
        "settings.languageAuto": "Otomatik",
        "settings.theme": "Görünüm",
        "theme.system": "Sistem",
        "theme.light": "Açık",
        "theme.dark": "Koyu",
        "settings.install": "Uygulamayı yükle",
        "settings.installHint": "Hızlı erişim için Subbed yönetimini cihazına ekle.",
        "settings.installManual": "Tarayıcı menüsünden “Yükle” veya “Ana Ekrana Ekle” seçeneğini kullan.",
        "settings.installed": "Yüklü uygulama olarak çalışıyor",
        "status.offline": "Çevrimdışısın. Uygulama açılır ama kütüphane ve yüklemeler bağlantı ister.",
        "status.update": "Yeni sürüm hazır.",
        "status.reload": "Yenile",
        "error.network": "Sunucuya ulaşılamıyor",
        "error.http": "İstek başarısız ({status})",
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
 * Look up a message and fill `{placeholders}`. With `params.n`, picks the ".one" or ".other" variant.
 * @param {Language} language
 * @param {string} key
 * @param {Record<string, string | number>} [params]
 * @returns {string}
 */
export function translate(language, key, params = {}) {
    let resolvedKey = key;
    if (typeof params.n === "number") {
        resolvedKey = `${key}.${new Intl.PluralRules(language).select(params.n) === "one" ? "one" : "other"}`;
    }
    const template = messages[language][resolvedKey] ?? messages.en[resolvedKey] ?? key;
    return template.replace(/\{(\w+)\}/g, (placeholder, name) => String(params[name] ?? placeholder));
}
