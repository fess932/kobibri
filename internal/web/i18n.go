package web

import (
	"net/http"
	"strings"
)

// Lang is a UI language. The server itself — logs, errors, the API — is always
// English; only what a person reads in the browser is translated.
type Lang string

const (
	LangEN Lang = "en"
	LangRU Lang = "ru"

	// DefaultLang is used when nothing else applies.
	DefaultLang = LangEN

	langCookie = "kobibri_lang"
)

// Languages lists what a person can pick, in menu order.
var Languages = []struct {
	Code Lang
	Name string
}{
	{LangEN, "English"},
	{LangRU, "Русский"},
}

func validLang(s string) (Lang, bool) {
	for _, l := range Languages {
		if string(l.Code) == s {
			return l.Code, true
		}
	}
	return "", false
}

// langOf picks the language for a request: an explicit choice first, then what
// the browser asks for, then English.
func langOf(r *http.Request) Lang {
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := validLang(c.Value); ok {
			return l
		}
	}
	// Accept-Language is a ranked list; take the first tag we actually have.
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		base, _, _ := strings.Cut(tag, "-")
		if l, ok := validLang(strings.ToLower(base)); ok {
			return l
		}
	}
	return DefaultLang
}

func setLangCookie(w http.ResponseWriter, lang Lang) {
	http.SetCookie(w, &http.Cookie{
		Name: langCookie, Value: string(lang), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600,
	})
}

// T looks up a phrase. A missing translation falls back to English rather than
// showing the key, so a half-finished catalogue degrades quietly.
//
// A phrase that has to name something — a library, an error — carries that value
// after a separator, put there by Msg. Keeping key and value in one string is
// what lets a flash message survive the round trip through a redirect and still
// be translated on the far side.
func T(lang Lang, key string) string {
	if s, ok := lookup(lang, key); ok {
		return s
	}
	if base, arg, found := strings.Cut(key, argSep); found {
		if s, ok := lookup(lang, base); ok {
			return strings.ReplaceAll(s, "%s", arg)
		}
	}
	return key
}

// Msg pairs a phrase with the value it names.
func Msg(key, arg string) string { return key + argSep + arg }

const argSep = "\x1f"

func lookup(lang Lang, key string) (string, bool) {
	m, ok := catalog[key]
	if !ok {
		return "", false
	}
	if s, ok := m[lang]; ok && s != "" {
		return s, true
	}
	s, ok := m[LangEN]
	return s, ok
}

// catalog holds every phrase the browser interface shows. English is the
// source; a key with no Russian entry falls back to it.
var catalog = map[string]map[Lang]string{
	// Chrome
	"app.tagline":  {LangEN: "Your Calibre shelves, on your Kobo.", LangRU: "Ваши полки Calibre — на вашей Kobo."},
	"nav.overview": {LangEN: "Overview", LangRU: "Обзор"},
	"nav.library":  {LangEN: "Library", LangRU: "Библиотека"},
	"nav.readers":  {LangEN: "Readers", LangRU: "Читалки"},
	"nav.sources":  {LangEN: "Libraries", LangRU: "Источники"},
	"nav.people":   {LangEN: "People", LangRU: "Люди"},
	"nav.signout":  {LangEN: "Sign out", LangRU: "Выйти"},
	"nav.language": {LangEN: "Language", LangRU: "Язык"},
	"nav.imports":  {LangEN: "From the web", LangRU: "Из интернета"},
	"nav.uploads":  {LangEN: "Your own files", LangRU: "Свои файлы"},

	// Uploading files by hand
	"uploads.title": {LangEN: "Your own files", LangRU: "Свои файлы"},
	"uploads.lede": {
		LangEN: "Books you put here yourself, without going through Calibre. When the same book is in a Calibre library too, this copy is the one that reaches your reader.",
		LangRU: "Книги, которые вы кладёте сюда сами, минуя Calibre. Если та же книга есть и в источнике Calibre, на читалку уедет именно эта копия.",
	},
	"uploads.disabled": {LangEN: "Uploading is switched off on this server.", LangRU: "Загрузка файлов на этом сервере выключена."},
	"uploads.add":      {LangEN: "Add books", LangRU: "Добавить книги"},
	"uploads.files":    {LangEN: "Files", LangRU: "Файлы"},
	"uploads.send":     {LangEN: "Upload", LangRU: "Загрузить"},
	"uploads.hint": {
		LangEN: "Several at once is fine. Anything not already an EPUB is converted first. What this server can convert:",
		LangRU: "Можно несколько сразу. Всё, что не EPUB, сначала конвертируется. Что этот сервер умеет:",
	},
	"uploads.noCalibre": {
		LangEN: "Kindle and other formats need Calibre installed on the server; without it they are stored but never reach a reader.",
		LangRU: "Форматы Kindle и прочие требуют установленной на сервере Calibre; без неё они сохранятся, но на читалку не попадут.",
	},
	"uploads.here":       {LangEN: "Here already", LangRU: "Уже загружены"},
	"uploads.size":       {LangEN: "Size", LangRU: "Размер"},
	"uploads.remove":     {LangEN: "Remove", LangRU: "Удалить"},
	"uploads.removed":    {LangEN: "Removed", LangRU: "Удалена"},
	"uploads.none":       {LangEN: "Nothing uploaded yet.", LangRU: "Пока ничего не загружено."},
	"uploads.removeHint": {LangEN: "Removing deletes the file here. Books already on a reader stay there.", LangRU: "Удаление стирает файл здесь. Книги, уже загруженные на читалку, останутся там."},
	"upload.badFormat":   {LangEN: "a Kobo cannot read this kind of file", LangRU: "такой файл Kobo не прочитает"},
	"upload.tooLarge":    {LangEN: "too large", LangRU: "слишком большой"},
	"upload.empty":       {LangEN: "the file is empty", LangRU: "файл пустой"},

	// Importing from a link
	"imports.title": {LangEN: "Books from the web", LangRU: "Книги из интернета"},
	"imports.lede": {
		LangEN: "Paste a link to a title and pick which translation to download. It is filed as an ordinary book, so it converts and syncs like the rest.",
		LangRU: "Вставьте ссылку на тайтл и выберите перевод. Книга попадёт в библиотеку как обычная — так же конвертируется и синкается.",
	},
	"imports.disabled": {LangEN: "Importing from the web is switched off on this server.", LangRU: "Импорт из интернета на этом сервере выключен."},
	"imports.add":      {LangEN: "Add a book", LangRU: "Добавить книгу"},
	"imports.link":     {LangEN: "Link", LangRU: "Ссылка"},
	"imports.lookUp":   {LangEN: "Look up", LangRU: "Найти"},
	"imports.hint": {
		LangEN: "Nothing is downloaded yet — the link is only read to see which translations exist.",
		LangRU: "Пока ничего не скачивается — ссылка читается только чтобы узнать, какие есть переводы.",
	},
	"imports.token":      {LangEN: "Access token", LangRU: "Токен доступа"},
	"imports.tokenField": {LangEN: "Token", LangRU: "Токен"},
	"imports.tokenLede": {
		LangEN: "Some titles are invisible to anyone not signed in, and the site answers exactly as it does for a book that never existed. A token makes them visible.",
		LangRU: "Некоторые тайтлы не видны без входа в аккаунт, и сайт отвечает ровно так же, как на несуществующую книгу. С токеном они становятся видны.",
	},
	"imports.tokenHint": {
		LangEN: "The site's own access token, copied from a browser already signed in. kobibri never sees your password and does not sign in for you. Leave it empty and save to remove it.",
		LangRU: "Собственный токен сайта, скопированный из браузера, где вы уже вошли. kobibri не видит ваш пароль и не входит за вас. Чтобы удалить, сохраните пустое поле.",
	},
	"imports.tokenSet":   {LangEN: "a token is stored — type a new one to replace it", LangRU: "токен сохранён — введите новый, чтобы заменить"},
	"imports.tokenEmpty": {LangEN: "no token", LangRU: "токена нет"},

	"imports.chooseTranslation": {LangEN: "Choose a translation", LangRU: "Выберите перевод"},
	"imports.chooseLede": {
		LangEN: "These are different texts, not different files: the wording differs, and often the chapter numbering too.",
		LangRU: "Это разные тексты, а не разные файлы: отличается формулировка, а часто и нумерация глав.",
	},
	"imports.translation": {LangEN: "Translation", LangRU: "Перевод"},
	"imports.team":        {LangEN: "Team", LangRU: "Команда"},
	"imports.chapters":    {LangEN: "Chapters", LangRU: "Главы"},
	"imports.download":    {LangEN: "Download", LangRU: "Скачать"},
	"imports.empty":       {LangEN: "nothing published", LangRU: "нет глав"},
	"imports.inProgress":  {LangEN: "Downloading now", LangRU: "Скачивается сейчас"},
	"imports.progress":    {LangEN: "Progress", LangRU: "Прогресс"},
	"imports.downloading": {LangEN: "Downloading", LangRU: "Скачивается"},
	"imports.imported":    {LangEN: "Already here", LangRU: "Уже загружены"},
	"imports.checkForNew": {LangEN: "Check for new chapters", LangRU: "Проверить новые главы"},
	"imports.none":        {LangEN: "Nothing has been imported yet.", LangRU: "Пока ничего не импортировано."},
	"imports.autoRefresh": {LangEN: "This page refreshes itself while a download is running.", LangRU: "Пока идёт загрузка, страница обновляется сама."},

	// Login
	"login.title":     {LangEN: "Sign in", LangRU: "Вход"},
	"login.name":      {LangEN: "Name", LangRU: "Имя"},
	"login.password":  {LangEN: "Password", LangRU: "Пароль"},
	"login.submit":    {LangEN: "Sign in", LangRU: "Войти"},
	"login.failed":    {LangEN: "That username and password do not match.", LangRU: "Такое имя и пароль не подходят."},
	"login.nosession": {LangEN: "Could not start a session. Try again.", LangRU: "Не удалось начать сессию. Попробуйте ещё раз."},

	// Dashboard
	"dash.title":         {LangEN: "Overview", LangRU: "Обзор"},
	"dash.lede":          {LangEN: "What is on the server, and what is on your readers.", LangRU: "Что есть на сервере и что — на ваших читалках."},
	"dash.setup":         {LangEN: "Set up a reader", LangRU: "Подключить читалку"},
	"dash.books":         {LangEN: "Books", LangRU: "Книги"},
	"dash.readyForKobo":  {LangEN: "ready for Kobo", LangRU: "готовы для Kobo"},
	"dash.sources":       {LangEN: "Libraries", LangRU: "Источники"},
	"dash.allHealthy":    {LangEN: "all healthy", LangRU: "все в порядке"},
	"dash.needAttention": {LangEN: "need attention", LangRU: "требуют внимания"},
	"dash.readers":       {LangEN: "Readers", LangRU: "Читалки"},
	"dash.collections":   {LangEN: "collection(s)", LangRU: "коллекций"},
	"dash.converted":     {LangEN: "Converted", LangRU: "Сконвертировано"},
	"dash.onDisk":        {LangEN: "on disk", LangRU: "на диске"},
	"dash.gone":          {LangEN: "No longer on disk", LangRU: "Пропали с диска"},
	"dash.goneSafe":      {LangEN: "still safe on your readers", LangRU: "но на читалках сохранены"},
	"dash.whereFrom":     {LangEN: "Where the books come from.", LangRU: "Откуда берутся книги."},
	"dash.manage":        {LangEN: "Manage", LangRU: "Настроить"},
	"dash.devicesLede":   {LangEN: "Kobo devices that have talked to this server.", LangRU: "Устройства Kobo, которые обращались к серверу."},
	"dash.noDevices":     {LangEN: "No reader has connected yet.", LangRU: "Ни одна читалка ещё не подключалась."},
	"dash.setUpOne":      {LangEN: "Set one up", LangRU: "Подключить"},
	"dash.recent":        {LangEN: "Recently added", LangRU: "Недавно добавленные"},
	"dash.allBooks":      {LangEN: "All books", LangRU: "Все книги"},

	// Dashboard warnings. %s is the library's name, or a count.
	"warn.unreachable": {
		LangEN: "%s cannot be reached. Nothing was changed — books already on your Kobo are safe.",
		LangRU: "%s недоступен. Ничего не изменено — книги, уже загруженные на Kobo, в порядке.",
	},
	"warn.unreachable.action": {LangEN: "Check the source", LangRU: "Проверить источник"},
	"warn.suspicious": {
		LangEN: "%s looks like it lost most of its books. The scan was refused until you confirm.",
		LangRU: "Похоже, источник %s потерял большую часть книг. Скан отклонён, пока вы не подтвердите.",
	},
	"warn.suspicious.action": {LangEN: "Review and confirm", LangRU: "Проверить и подтвердить"},
	"warn.scanFailed":        {LangEN: "%s failed to scan.", LangRU: "Не удалось просканировать источник %s."},
	"warn.scanFailed.action": {LangEN: "Open sources", LangRU: "Открыть источники"},
	"warn.noSources": {
		LangEN: "No libraries yet. Add the folder that holds your Calibre metadata.db.",
		LangRU: "Источников пока нет. Добавьте папку, где лежит ваш metadata.db от Calibre.",
	},
	"warn.noSources.action": {LangEN: "Add a library", LangRU: "Добавить источник"},
	"warn.unconverted": {
		LangEN: "%s book(s) could not be converted. They are served as plain EPUB, which reads fine but tracks progress by chapter only.",
		LangRU: "Не удалось сконвертировать книг: %s. Они отдаются обычным EPUB — читаются нормально, но прогресс считается только по главам.",
	},
	"warn.unconverted.action": {LangEN: "See the library", LangRU: "Показать книги"},

	// Table headings
	"th.name":            {LangEN: "Name", LangRU: "Название"},
	"th.status":          {LangEN: "Status", LangRU: "Состояние"},
	"th.books":           {LangEN: "Books", LangRU: "Книги"},
	"th.priority":        {LangEN: "Priority", LangRU: "Приоритет"},
	"th.lastScan":        {LangEN: "Last scan", LangRU: "Последний скан"},
	"th.folder":          {LangEN: "Folder", LangRU: "Папка"},
	"th.reader":          {LangEN: "Reader", LangRU: "Читалка"},
	"th.owner":           {LangEN: "Owner", LangRU: "Владелец"},
	"th.firmware":        {LangEN: "Firmware", LangRU: "Прошивка"},
	"th.lastSync":        {LangEN: "Last sync", LangRU: "Последний синк"},
	"th.deletedOnDevice": {LangEN: "Deleted on device", LangRU: "Удалено на устройстве"},
	"th.when":            {LangEN: "When", LangRU: "Когда"},
	"th.result":          {LangEN: "Result", LangRU: "Результат"},
	"th.seen":            {LangEN: "Seen", LangRU: "Найдено"},
	"th.new":             {LangEN: "New", LangRU: "Новых"},
	"th.updated":         {LangEN: "Updated", LangRU: "Обновлено"},
	"th.gone":            {LangEN: "Gone", LangRU: "Пропало"},
	"th.role":            {LangEN: "Role", LangRU: "Роль"},
	"th.since":           {LangEN: "Since", LangRU: "С"},
	"th.changePassword":  {LangEN: "Change password", LangRU: "Сменить пароль"},
	"th.key":             {LangEN: "Key", LangRU: "Ключ"},
	"th.created":         {LangEN: "Created", LangRU: "Создан"},
	"th.lastUsed":        {LangEN: "Last used", LangRU: "Использован"},
	"th.book":            {LangEN: "Book", LangRU: "Книга"},
	"th.hasBook":         {LangEN: "Has this book", LangRU: "Есть эта книга"},
	"th.reading":         {LangEN: "Reading", LangRU: "Чтение"},
	"th.library":         {LangEN: "Library", LangRU: "Источник"},
	"th.titleThere":      {LangEN: "Title there", LangRU: "Название там"},
	"th.formats":         {LangEN: "Formats", LangRU: "Форматы"},
	"th.state":           {LangEN: "State", LangRU: "Роль"},

	// Status pills
	"pill.off":             {LangEN: "Off", LangRU: "Выключен"},
	"pill.healthy":         {LangEN: "Healthy", LangRU: "В порядке"},
	"pill.scanning":        {LangEN: "Scanning", LangRU: "Сканируется"},
	"pill.unreachable":     {LangEN: "Unreachable", LangRU: "Недоступен"},
	"pill.needsConfirming": {LangEN: "Needs confirming", LangRU: "Нужно подтвердить"},
	"pill.failed":          {LangEN: "Failed", LangRU: "Ошибка"},
	"pill.notScanned":      {LangEN: "Not scanned", LangRU: "Не сканировался"},
	"pill.hidden":          {LangEN: "Hidden", LangRU: "Скрыта"},
	"pill.notOnDisk":       {LangEN: "Not on disk", LangRU: "Нет на диске"},
	"pill.converting":      {LangEN: "Converting", LangRU: "Конвертируется"},
	"pill.libraries":       {LangEN: "libraries", LangRU: "источника"},
	"pill.readyForKobo":    {LangEN: "Ready for Kobo", LangRU: "Готова для Kobo"},
	"pill.cannotSync":      {LangEN: "Cannot sync", LangRU: "Не синкается"},
	"pill.done":            {LangEN: "Done", LangRU: "Готово"},
	"pill.running":         {LangEN: "Running", LangRU: "Идёт"},
	"pill.revoked":         {LangEN: "Revoked", LangRU: "Отозван"},
	"pill.you":             {LangEN: "you", LangRU: "это вы"},
	"pill.admin":           {LangEN: "Administrator", LangRU: "Администратор"},
	"pill.reader":          {LangEN: "Reader", LangRU: "Читатель"},
	"pill.suppliesRecord":  {LangEN: "Supplies the record", LangRU: "Даёт запись"},
	"pill.standby":         {LangEN: "Standby", LangRU: "Запасной"},
	"pill.goneFromLibrary": {LangEN: "Gone from this library", LangRU: "Пропала из источника"},
	"pill.cover":           {LangEN: "Cover", LangRU: "Обложка"},
	"pill.deletedOnDevice": {LangEN: "Deleted on the device", LangRU: "Удалена на устройстве"},
	"pill.yes":             {LangEN: "Yes", LangRU: "Да"},
	"pill.notYet":          {LangEN: "Not yet", LangRU: "Ещё нет"},
	"pill.fixed":           {LangEN: "fixed", LangRU: "фикс. вёрстка"},
	"pill.fileGone":        {LangEN: "file gone", LangRU: "файл пропал"},

	// Sources page
	"sources.title": {LangEN: "Libraries", LangRU: "Источники"},
	"sources.lede": {
		LangEN: "Folders kobibri reads. Calibre is never written to — kobibri works on a copy of metadata.db, so it is safe to scan while Calibre is open.",
		LangRU: "Папки, которые читает kobibri. В Calibre он никогда не пишет — работает с копией metadata.db, поэтому сканировать можно и при открытом Calibre.",
	},
	"sources.add":       {LangEN: "Add a library", LangRU: "Добавить источник"},
	"sources.folder":    {LangEN: "Folder", LangRU: "Папка"},
	"sources.scanEvery": {LangEN: "Scan every", LangRU: "Сканировать каждые"},
	"sources.addBtn":    {LangEN: "Add", LangRU: "Добавить"},
	"sources.hint": {
		LangEN: "Point this at the folder holding metadata.db. When two libraries hold the same book, the lower priority wins — but a library that actually has the file always beats one that does not. Interval is in seconds.",
		LangRU: "Укажите папку, где лежит metadata.db. Если книга есть в двух источниках, побеждает меньший приоритет — но источник, у которого файл реально есть, всегда бьёт тот, у которого его нет. Интервал в секундах.",
	},
	"sources.scanNow":        {LangEN: "Scan now", LangRU: "Сканировать"},
	"sources.confirmRemoval": {LangEN: "Confirm removal", LangRU: "Подтвердить удаление"},
	"sources.switchOff":      {LangEN: "Switch off", LangRU: "Выключить"},
	"sources.switchOn":       {LangEN: "Switch on", LangRU: "Включить"},
	"sources.remove":         {LangEN: "Remove", LangRU: "Удалить"},
	"sources.save":           {LangEN: "Save", LangRU: "Сохранить"},
	"sources.lastGoodScan":   {LangEN: "Last good scan", LangRU: "Последний удачный скан"},
	"sources.scansEvery":     {LangEN: "Scans every", LangRU: "Сканируется каждые"},
	"sources.recentScans":    {LangEN: "Recent scans", LangRU: "Недавние сканы"},
	"sources.none":           {LangEN: "No libraries yet. Add one above.", LangRU: "Источников пока нет. Добавьте выше."},

	// Who may see a library
	"sharing.title": {LangEN: "Who can see this library", LangRU: "Кому видна эта библиотека"},
	"sharing.lede": {
		LangEN: "Everyone by default. Restricting it affects what each person's readers receive from the next sync onwards — books already on a reader stay there, as always.",
		LangRU: "По умолчанию всем. Ограничение влияет на то, что получат читалки каждого начиная со следующего синка; книги, уже загруженные на читалку, останутся там, как всегда.",
	},
	"sharing.everyone": {LangEN: "Everyone", LangRU: "Всем"},
	"sharing.only":     {LangEN: "Only the people ticked below", LangRU: "Только отмеченным ниже"},

	// A library's own custom columns
	"columns.title": {LangEN: "Shelves from this library's own columns", LangRU: "Полки из собственных колонок источника"},
	"columns.lede": {
		LangEN: "Calibre columns you added yourself — a shelf, a reading status, a mood. Each value becomes a collection on your reader. They are not sent as the book's genre: a Kobo only understands its own store's categories there.",
		LangRU: "Колонки, которые вы завели в Calibre сами, — полка, статус, настроение. Каждое значение станет коллекцией на читалке. Жанром они не отправляются: там Kobo понимает только категории собственного магазина.",
	},

	// Collections built from the library's own organisation
	"collections.title": {LangEN: "Shelves on your reader", LangRU: "Полки на читалке"},
	"collections.lede": {
		LangEN: "Kobo calls them collections. This server can keep them in step with what Calibre already knows about your books.",
		LangRU: "На Kobo это коллекции. Сервер может держать их в согласии с тем, что Calibre уже знает о ваших книгах.",
	},
	"collections.build":  {LangEN: "Build shelves from", LangRU: "Собирать полки из"},
	"collections.off":    {LangEN: "Nothing — only shelves I make on the reader", LangRU: "Ничего — только полки, созданные на читалке"},
	"collections.tags":   {LangEN: "Calibre tags", LangRU: "Тегов Calibre"},
	"collections.series": {LangEN: "Series", LangRU: "Серий"},
	"collections.both":   {LangEN: "Tags and series", LangRU: "Тегов и серий"},
	"collections.hint": {
		LangEN: "Off by default: a library with two hundred tags would put two hundred shelves on your reader. A shelf you delete on the reader stays deleted.",
		LangRU: "По умолчанию выключено: библиотека с двумя сотнями тегов создаст на читалке две сотни полок. Полка, удалённая на читалке, останется удалённой.",
	},
	"sources.suspicious": {
		LangEN: "This scan would have marked an unusual number of books as gone, which usually means a half-mounted drive rather than a real deletion. Nothing was changed. If the library really did shrink, use Confirm removal.",
		LangRU: "Этот скан пометил бы пропавшими необычно много книг — обычно это наполовину примонтированный диск, а не настоящее удаление. Ничего не изменено. Если источник действительно уменьшился, нажмите «Подтвердить удаление».",
	},
	"sources.confirmDelete": {
		LangEN: "Books already on your readers stay there, and adding this library back later changes nothing on them.",
		LangRU: "Книги, уже загруженные на читалки, останутся там, и повторное добавление источника ничего на них не изменит.",
	},

	// Library page
	"library.title":           {LangEN: "Library", LangRU: "Библиотека"},
	"library.search":          {LangEN: "Search", LangRU: "Поиск"},
	"library.searchHint":      {LangEN: "Title, author or series", LangRU: "Название, автор или серия"},
	"library.any":             {LangEN: "Any", LangRU: "Любой"},
	"library.show":            {LangEN: "Show", LangRU: "Показать"},
	"library.everything":      {LangEN: "Everything", LangRU: "Всё"},
	"library.onlySyncable":    {LangEN: "Ready for Kobo", LangRU: "Готовые для Kobo"},
	"library.onlyGone":        {LangEN: "No longer on disk", LangRU: "Пропавшие с диска"},
	"library.onlyHidden":      {LangEN: "Hidden", LangRU: "Скрытые"},
	"library.onlyUnconverted": {LangEN: "Not converted yet", LangRU: "Ещё не сконвертированы"},
	"library.onlyReading":     {LangEN: "Being read", LangRU: "Читаются"},
	"library.onlyFinished":    {LangEN: "Finished", LangRU: "Прочитаны"},
	"read.progress":           {LangEN: "Read", LangRU: "Прочитано"},
	"read.finished":           {LangEN: "Finished", LangRU: "Прочитана"},
	"read.notStarted":         {LangEN: "Not started", LangRU: "Не начата"},
	"read.lastRead":           {LangEN: "Last read", LangRU: "Последнее чтение"},
	"library.sort":            {LangEN: "Order", LangRU: "Порядок"},
	"library.newestFirst":     {LangEN: "Newest first", LangRU: "Сначала новые"},
	"library.byTitle":         {LangEN: "By title", LangRU: "По названию"},
	"library.filter":          {LangEN: "Filter", LangRU: "Фильтр"},
	"library.clear":           {LangEN: "Clear", LangRU: "Сбросить"},
	"library.previous":        {LangEN: "Previous", LangRU: "Назад"},
	"library.next":            {LangEN: "Next", LangRU: "Вперёд"},
	"library.page":            {LangEN: "Page", LangRU: "Страница"},
	"library.of":              {LangEN: "of", LangRU: "из"},
	"library.nothing":         {LangEN: "Nothing matches that.", LangRU: "Ничего не нашлось."},
	"library.clearFilters":    {LangEN: "Clear the filters", LangRU: "Сбросить фильтры"},
	"library.empty":           {LangEN: "No books yet. Add a library on the Libraries page.", LangRU: "Книг пока нет. Добавьте источник на странице «Источники»."},

	// Book page
	"book.back":         {LangEN: "Library", LangRU: "Библиотека"},
	"book.download":     {LangEN: "Download", LangRU: "Скачать"},
	"book.downloadLede": {LangEN: "Only KEPUB is sent to a Kobo. The others are here for you.", LangRU: "На Kobo уходит только KEPUB. Остальные — для вас."},
	"book.noFile":       {LangEN: "No file for this book is on disk right now.", LangRU: "Файла этой книги сейчас нет на диске."},
	"book.asItIsIn":     {LangEN: "as it is in %s", LangRU: "как есть в источнике %s"},
	"book.convertedFor": {LangEN: "converted for Kobo — this is what syncs", LangRU: "сконвертировано для Kobo — именно это синкается"},
	"book.delete":       {LangEN: "Delete for good", LangRU: "Удалить насовсем"},
	"book.deleteConfirm": {
		LangEN: "Delete this book, its files and everything known about it? A reader that already has it keeps its copy, and importing the book again will bring it back as a new book.",
		LangRU: "Удалить книгу, её файлы и всё, что о ней известно? Читалка, на которой она уже есть, свою копию сохранит, а повторный импорт приведёт её как новую книгу.",
	},
	"book.deleteHint": {
		LangEN: "Deleting for good is for starting over — it frees the book to be imported again from scratch. Hiding is the reversible one, and the one that takes a book off a reader.",
		LangRU: "«Удалить насовсем» — это чтобы начать заново: книгу можно будет импортировать с нуля. Обратимый вариант — «скрыть», и именно он убирает книгу с читалки.",
	},
	"book.kepubNameHint": {
		LangEN: "A KEPUB is saved as .kepub.epub, and the name matters: a Kobo picks its reader by it, and only that name gets word-level progress. Keep it if you are copying the file to a Kobo over USB; take the EPUB if you are reading it anywhere else.",
		LangRU: "KEPUB сохраняется как .kepub.epub, и имя здесь значимое: Kobo по нему выбирает движок чтения, и только с таким именем работает прогресс по словам. Оставьте его, если копируете файл на Kobo по USB; для чтения где-то ещё берите EPUB.",
	},
	"book.alreadyKepub": {
		LangEN: "already a KEPUB in %s — this is what syncs",
		LangRU: "уже KEPUB в источнике %s — именно это синкается",
	},
	"book.convertFailed": {
		LangEN: "This book could not be converted, so the plain EPUB is served instead. It reads fine; only mid-chapter reading position is lost.",
		LangRU: "Эту книгу не удалось сконвертировать, поэтому отдаётся обычный EPUB. Читается нормально; теряется только позиция чтения внутри главы.",
	},
	"book.whereFrom": {LangEN: "Where it comes from", LangRU: "Откуда она"},
	"book.whereFromLede": {
		LangEN: "Listed in the order kobibri picks a winner. The top live row supplies the record.",
		LangRU: "В том порядке, в каком kobibri выбирает победителя. Верхняя живая строка даёт запись.",
	},
	"book.onReaders":    {LangEN: "On your readers", LangRU: "На ваших читалках"},
	"book.noSync":       {LangEN: "No reader has synced yet.", LangRU: "Ни одна читалка ещё не синхронизировалась."},
	"book.details":      {LangEN: "Details", LangRU: "Подробности"},
	"book.publisher":    {LangEN: "Publisher", LangRU: "Издатель"},
	"book.language":     {LangEN: "Language", LangRU: "Язык"},
	"book.sendsAs":      {LangEN: "Sends as", LangRU: "Отдаётся как"},
	"book.cannotRead":   {LangEN: "nothing — Kobo cannot read this", LangRU: "никак — Kobo это не читает"},
	"book.identifier":   {LangEN: "Identifier", LangRU: "Идентификатор"},
	"book.revision":     {LangEN: "Revision", LangRU: "Ревизия"},
	"book.takeOff":      {LangEN: "Take off readers", LangRU: "Убрать с читалок"},
	"book.showAgain":    {LangEN: "Show on readers again", LangRU: "Вернуть на читалки"},
	"book.convertAgain": {LangEN: "Convert again", LangRU: "Сконвертировать заново"},
	"book.hideHint": {
		LangEN: "Taking a book off your readers moves it to their archive on the next sync. It stays in the library here.",
		LangRU: "Убранная книга уедет в архив читалки при следующем синке. В библиотеке здесь она останется.",
	},

	// Devices page
	"devices.title": {LangEN: "Readers", LangRU: "Читалки"},
	"devices.lede": {
		LangEN: "Each Kobo gets its own key. Revoking one stops that reader and leaves the rest alone.",
		LangRU: "У каждой Kobo свой ключ. Отзыв одного останавливает только эту читалку, остальные не трогает.",
	},
	"devices.setupTitle": {LangEN: "Point your Kobo at this server", LangRU: "Направьте Kobo на этот сервер"},
	"devices.setupBody": {
		LangEN: "Plug the Kobo in by USB and open .kobo/Kobo/Kobo eReader.conf. Under [OneStoreServices], replace the api_endpoint line with this one, then eject and restart the reader.",
		LangRU: "Подключите Kobo по USB и откройте .kobo/Kobo/Kobo eReader.conf. В разделе [OneStoreServices] замените строку api_endpoint на эту, затем извлеките устройство и перезагрузите его.",
	},
	"devices.copy":   {LangEN: "Copy", LangRU: "Копировать"},
	"devices.copied": {LangEN: "Copied", LangRU: "Скопировано"},
	"devices.backupWarning": {
		LangEN: "Back that file up first. The Kobo remembers what it is told here permanently, so a mistake has to be undone by hand. This key is shown once and cannot be looked up again.",
		LangRU: "Сначала сделайте резервную копию этого файла. Kobo запоминает то, что здесь указано, навсегда, и ошибку придётся исправлять вручную. Ключ показывается один раз и больше не восстанавливается.",
	},
	"devices.baseHint": {
		LangEN: "The address above was guessed from your browser. If the Kobo cannot reach it, set KOBIBRI_BASE_URL — and if the reader fails on a hostname, use the server's IP address instead.",
		LangRU: "Адрес выше угадан по вашему браузеру. Если Kobo до него не достучится, задайте KOBIBRI_BASE_URL — а если читалка не резолвит имя, используйте IP-адрес сервера.",
	},
	"devices.keys":          {LangEN: "Keys", LangRU: "Ключи"},
	"devices.onePerReader":  {LangEN: "One per reader.", LangRU: "По одному на читалку."},
	"devices.whichReader":   {LangEN: "Which reader is this for?", LangRU: "Для какой читалки?"},
	"devices.createKey":     {LangEN: "Create a key", LangRU: "Создать ключ"},
	"devices.revoke":        {LangEN: "Revoke", LangRU: "Отозвать"},
	"devices.confirmRevoke": {LangEN: "Revoke this key? That reader stops syncing until it is set up again.", LangRU: "Отозвать ключ? Эта читалка перестанет синхронизироваться, пока её не настроят заново."},
	"devices.resend":        {LangEN: "Resend everything", LangRU: "Отправить всё заново"},
	"devices.confirmResend": {
		LangEN: "Send the whole library to this reader again? Books you deleted on it stay deleted.",
		LangRU: "Отправить всю библиотеку на эту читалку заново? Книги, удалённые на ней, останутся удалёнными.",
	},
	"devices.deviceID":    {LangEN: "Device id", LangRU: "ID устройства"},
	"devices.deletedHere": {LangEN: "Deleted on this reader", LangRU: "Удалено на этой читалке"},
	"devices.deletedHint": {
		LangEN: "These stay in the library and on your other readers. They are simply never sent to this one again.",
		LangRU: "Они остаются в библиотеке и на других читалках. Просто на эту больше не отправляются.",
	},
	"devices.sendBack": {LangEN: "Send it back", LangRU: "Вернуть"},
	"devices.none":     {LangEN: "No reader has connected yet. Create a key above and follow the instructions.", LangRU: "Ни одна читалка ещё не подключалась. Создайте ключ выше и следуйте инструкции."},
	"devices.unnamed":  {LangEN: "Unnamed reader", LangRU: "Читалка без имени"},
	"devices.unknown":  {LangEN: "unknown", LangRU: "неизвестна"},
	"devices.firmware": {LangEN: "firmware", LangRU: "прошивка"},
	"devices.lastSeen": {LangEN: "last seen", LangRU: "видели"},

	// Users page
	"users.title": {LangEN: "People", LangRU: "Люди"},
	"users.lede": {
		LangEN: "Everyone with an account here. Each keeps their own readers, reading progress and collections.",
		LangRU: "Все, у кого здесь есть учётная запись. У каждого свои читалки, прогресс чтения и коллекции.",
	},
	"users.add":         {LangEN: "Add someone", LangRU: "Добавить человека"},
	"users.isAdmin":     {LangEN: "Administrator", LangRU: "Администратор"},
	"users.addBtn":      {LangEN: "Add", LangRU: "Добавить"},
	"users.yes":         {LangEN: "Yes", LangRU: "Да"},
	"users.no":          {LangEN: "No", LangRU: "Нет"},
	"users.hint":        {LangEN: "At least 8 characters. Administrators can manage libraries and people.", LangRU: "Минимум 8 символов. Администраторы управляют источниками и людьми."},
	"users.accounts":    {LangEN: "Accounts", LangRU: "Учётные записи"},
	"users.newPassword": {LangEN: "New password", LangRU: "Новый пароль"},
	"users.set":         {LangEN: "Set", LangRU: "Задать"},
	"users.remove":      {LangEN: "Remove", LangRU: "Удалить"},
	"users.confirmRemove": {
		LangEN: "Their readers, reading progress and collections go too.",
		LangRU: "Вместе с ним удалятся его читалки, прогресс чтения и коллекции.",
	},

	// Flash messages. Anything with a name or a count in it stays literal and is
	// passed through untranslated.
	"flash.sourceNeedsNamePath":  {LangEN: "A library needs a name and a folder path.", LangRU: "Источнику нужны имя и путь к папке."},
	"flash.badPath":              {LangEN: "That path could not be resolved.", LangRU: "Не удалось разобрать этот путь."},
	"flash.sourceGone":           {LangEN: "That library no longer exists.", LangRU: "Такого источника больше нет."},
	"flash.suspicious":           {LangEN: "That scan would remove an unusual number of books. If the library really did shrink, use Confirm removal.", LangRU: "Этот скан удалил бы необычно много книг. Если источник действительно уменьшился, нажмите «Подтвердить удаление»."},
	"flash.unreachable":          {LangEN: "The library folder could not be read. Nothing was changed.", LangRU: "Папку источника не удалось прочитать. Ничего не изменено."},
	"flash.sourceOn":             {LangEN: "Library switched on.", LangRU: "Источник включён."},
	"flash.sourceOff":            {LangEN: "Library switched off. Books already on your Kobo stay there.", LangRU: "Источник выключен. Книги, уже загруженные на Kobo, останутся там."},
	"flash.sourceRemoved":        {LangEN: "Library removed. Its books keep their identity, so adding it back changes nothing on your Kobo.", LangRU: "Источник удалён. Книги сохраняют свою идентичность, поэтому повторное добавление ничего не изменит на Kobo."},
	"flash.hidden":               {LangEN: "Hidden. The next sync moves it to your Kobo's archive.", LangRU: "Скрыта. При следующем синке уедет в архив Kobo."},
	"flash.shown":                {LangEN: "Visible again. The next sync sends it back.", LangRU: "Снова видима. Следующий синк отправит её обратно."},
	"flash.converting":           {LangEN: "Converting again in the background.", LangRU: "Конвертируется заново в фоне."},
	"flash.tokenRevoked":         {LangEN: "Token revoked. That reader stops syncing; the others are unaffected.", LangRU: "Ключ отозван. Эта читалка перестала синхронизироваться, остальные не затронуты."},
	"flash.syncReset":            {LangEN: "Sync state cleared. The next sync sends the whole library again. Books you deleted on that reader stay deleted.", LangRU: "Состояние синка сброшено. Следующий синк отправит всю библиотеку заново. Книги, удалённые на этой читалке, останутся удалёнными."},
	"flash.tombstoneGone":        {LangEN: "The next sync sends that book back to the reader.", LangRU: "Следующий синк вернёт эту книгу на читалку."},
	"flash.notYours":             {LangEN: "That belongs to someone else.", LangRU: "Это принадлежит другому человеку."},
	"flash.needNamePassword":     {LangEN: "A person needs a name and a password of at least 8 characters.", LangRU: "Нужны имя и пароль не короче 8 символов."},
	"flash.shortPassword":        {LangEN: "Use a password of at least 8 characters.", LangRU: "Пароль должен быть не короче 8 символов."},
	"flash.passwordChanged":      {LangEN: "Password changed.", LangRU: "Пароль изменён."},
	"flash.cannotRemoveSelf":     {LangEN: "You cannot remove your own account.", LangRU: "Нельзя удалить собственную учётную запись."},
	"flash.lastAdmin":            {LangEN: "That is the only administrator; the server would be unmanageable.", LangRU: "Это единственный администратор — сервером станет некому управлять."},
	"flash.needLink":             {LangEN: "Paste a link to a book first.", LangRU: "Сначала вставьте ссылку на книгу."},
	"flash.importStarted":        {LangEN: "Downloading. It carries on in the background, so you can leave this page.", LangRU: "Скачивается. Загрузка идёт в фоне, страницу можно закрыть."},
	"flash.importAlreadyRunning": {LangEN: "That one is already downloading.", LangRU: "Эта книга уже скачивается."},
	"flash.checkingForChapters":  {LangEN: "Checking for new chapters.", LangRU: "Проверяю новые главы."},
	"flash.tokenSaved":           {LangEN: "Token saved. Titles that need an account are now visible.", LangRU: "Токен сохранён. Тайтлы, для которых нужен аккаунт, теперь видны."},
	"flash.tokenCleared":         {LangEN: "Token removed.", LangRU: "Токен удалён."},
	"flash.importsOff":           {LangEN: "Importing from the web is switched off.", LangRU: "Импорт из интернета выключен."},
	"flash.userRemoved":          {LangEN: "Account removed, along with its readers and reading progress.", LangRU: "Учётная запись удалена вместе с её читалками и прогрессом чтения."},

	// Flashes that name something. %s is filled in by Msg.
	"flash.noMetadataDb": {
		LangEN: "No metadata.db in %s. Point this at the folder Calibre keeps your library in.",
		LangRU: "В %s нет metadata.db. Укажите папку, в которой Calibre держит библиотеку.",
	},
	"flash.sourceAddFailed": {LangEN: "Could not add that library: %s", LangRU: "Не удалось добавить источник: %s"},
	"flash.sourceAdded":     {LangEN: "Added %s. Scanning it now.", LangRU: "Источник %s добавлен. Сканирую."},
	"flash.sourceSaved":     {LangEN: "Saved %s.", LangRU: "Источник %s сохранён."},
	"flash.userAdded":       {LangEN: "Added %s.", LangRU: "%s добавлен."},
	"flash.userAddFailed":   {LangEN: "Could not add %s", LangRU: "Не удалось добавить %s"},
	"flash.uploadsOff":      {LangEN: "Uploading is switched off on this server.", LangRU: "Загрузка файлов на этом сервере выключена."},
	"flash.uploadFailed":    {LangEN: "That upload could not be read.", LangRU: "Не удалось прочитать загрузку."},
	"flash.uploadNothing":   {LangEN: "No file was chosen.", LangRU: "Файл не выбран."},
	"flash.uploaded": {
		LangEN: "%s book(s) added. Converting them now, so your reader does not have to wait.",
		LangRU: "Добавлено книг: %s. Конвертирую — чтобы читалке не пришлось ждать.",
	},
	"flash.uploadedSome":     {LangEN: "%s book(s) added.", LangRU: "Добавлено книг: %s."},
	"flash.uploadFailedWith": {LangEN: "Not added: %s", LangRU: "Не добавлены: %s"},
	"flash.uploadRemoved": {
		LangEN: "Removed. Anything a reader already has stays on it.",
		LangRU: "Удалено. То, что уже есть на читалках, там и останется.",
	},
	"flash.deleted": {
		LangEN: "%s is gone, with its files. Readers that already have it keep their copies.",
		LangRU: "%s удалена вместе с файлами. На читалках, где она уже есть, копии останутся.",
	},
	"flash.deletedButInCalibre": {
		LangEN: "%s was forgotten, but it is still in a Calibre library — nothing there is ever deleted, so the next scan will bring it back. Remove it in Calibre first.",
		LangRU: "%s забыта, но она всё ещё в библиотеке Calibre — там мы ничего не удаляем, поэтому следующий скан вернёт её. Удалите её сначала в Calibre.",
	},
	"flash.deleteFailed": {LangEN: "Could not delete that book: %s", LangRU: "Не удалось удалить книгу: %s"},
	"flash.split": {
		LangEN: "Split. This is the copy that moved; the original kept its identity and whatever is on your readers.",
		LangRU: "Разделено. Это отделённая копия; исходная книга сохранила идентичность и всё, что есть на читалках.",
	},
	"flash.splitFailed":  {LangEN: "Could not split that copy off: %s", LangRU: "Не удалось отделить копию: %s"},
	"flash.rejoined":     {LangEN: "Put back with the book its title and author point at.", LangRU: "Возвращена к книге, на которую указывают её название и автор."},
	"flash.rejoinFailed": {LangEN: "Could not put that copy back: %s", LangRU: "Не удалось вернуть копию: %s"},
	"flash.sharingSaved": {
		LangEN: "Saved. Readers get the change on their next sync.",
		LangRU: "Сохранено. Читалки получат изменение при следующем синке.",
	},
	"flash.columnsSaved": {
		LangEN: "Saved. The library is being re-read to pick up those values; the shelves appear when it finishes.",
		LangRU: "Сохранено. Источник перечитывается ради этих значений — полки появятся, когда он закончит.",
	},
	"flash.collectionsSaved": {
		LangEN: "Saved. The shelves are rebuilt; your readers pick them up on the next sync.",
		LangRU: "Сохранено. Полки пересобраны — читалки получат их при следующем синке.",
	},

	// Errors shown as a bare page rather than in the interface.
	"err.tooLargeToConvert": {
		LangEN: "This book is too large to convert. Download the EPUB instead.",
		LangRU: "Эта книга слишком большая для конвертации. Скачайте EPUB.",
	},
	"err.couldNotConvert": {
		LangEN: "This book could not be converted. Download the EPUB instead.",
		LangRU: "Эту книгу не удалось сконвертировать. Скачайте EPUB.",
	},
	"err.pageFailed":  {LangEN: "Something went wrong loading this page.", LangRU: "Не удалось загрузить эту страницу."},
	"err.formExpired": {LangEN: "This form has expired. Reload the page and try again.", LangRU: "Форма устарела. Обновите страницу и попробуйте ещё раз."},
	"err.adminsOnly":  {LangEN: "This page is for administrators.", LangRU: "Эта страница только для администраторов."},

	// Reading in the browser
	"read.title": {LangEN: "Read", LangRU: "Читать"},
	"read.lede": {
		LangEN: "The converted file, as a reader would see it. Enough to check the chapters came out right.",
		LangRU: "Сконвертированный файл — примерно так его увидит читалка. Достаточно, чтобы проверить, что главы вышли нормально.",
	},
	"read.backToBook": {LangEN: "Back to the book", LangRU: "К книге"},
	"read.chapter":    {LangEN: "Chapter", LangRU: "Глава"},
	"read.go":         {LangEN: "Go", LangRU: "Перейти"},
	"read.hint": {
		LangEN: "This is not a reading app — it opens the file that syncs, so you can see what your Kobo will get.",
		LangRU: "Это не приложение для чтения — здесь открывается тот самый файл, который синкается, чтобы видеть, что получит Kobo.",
	},
	"read.noFile":     {LangEN: "There is no file to open for this book.", LangRU: "Для этой книги нет файла, который можно открыть."},
	"read.unreadable": {LangEN: "This file could not be opened: %s", LangRU: "Не удалось открыть файл: %s"},

	// The catalogue feed
	"opds.title": {LangEN: "Read on anything else", LangRU: "Читать на чём-то ещё"},
	"opds.lede": {
		LangEN: "A catalogue feed, for reading apps that are not a Kobo — KOReader, Foliate, Moon+ and the rest.",
		LangRU: "Каталог для читалок и приложений, кроме Kobo, — KOReader, Foliate, Moon+ и прочих.",
	},
	"opds.hint": {
		LangEN: "Add it as an OPDS catalogue and sign in with your kobibri name and password. You see the same books you see here.",
		LangRU: "Добавьте как OPDS-каталог и войдите со своим именем и паролем kobibri. Книги будут те же, что и здесь.",
	},

	// Books merged by mistake
	"dupes.title": {LangEN: "Possibly the same book twice", LangRU: "Возможно, одна книга дважды"},
	"dupes.lede": {
		LangEN: "Copies joined only because their title and author match — the one kind of merge that can be wrong.",
		LangRU: "Копии, объединённые только по совпадению названия и автора, — единственный вид склейки, который может быть неверным.",
	},
	"dupes.note": {
		LangEN: "Most of these are right: the same book in two libraries, neither carrying a uuid or an ISBN. Look for the ones where the copies are really different books — two translations, two anthologies with the same name, a reissue with new content.",
		LangRU: "Почти все они верны: одна книга в двух источниках, ни у одной нет uuid или ISBN. Ищите те, где копии — действительно разные книги: два перевода, два сборника с одинаковым названием, переиздание с другим содержанием.",
	},
	"dupes.split": {LangEN: "This is a different book", LangRU: "Это другая книга"},
	"dupes.splitHint": {
		LangEN: "Splitting keeps the original book's identity, so what is already on a reader stays. The copy that leaves becomes a new book and arrives as one. It will not be merged back by a later scan.",
		LangRU: "При разделении исходная книга сохраняет свою идентичность — то, что уже на читалке, там и останется. Отделённая копия становится новой книгой и приедет как новая. Следующий скан не склеит её обратно.",
	},
	"dupes.none":     {LangEN: "Nothing looks wrongly merged.", LangRU: "Ничего похожего на ошибочную склейку."},
	"dupes.check":    {LangEN: "Check for duplicates", LangRU: "Проверить дубликаты"},
	"book.splitOff":  {LangEN: "Split off", LangRU: "Отделена"},
	"book.rejoin":    {LangEN: "Put back", LangRU: "Вернуть"},
	"book.splitThis": {LangEN: "This is a different book", LangRU: "Это другая книга"},

	// Books
	"book.unknownAuthor": {LangEN: "Unknown author", LangRU: "Автор неизвестен"},
	"book.andOthers":     {LangEN: "and %s others", LangRU: "и ещё %s"},

	// Shared
	"common.never":         {LangEN: "never", LangRU: "никогда"},
	"common.unknownAuthor": {LangEN: "Unknown author", LangRU: "Автор неизвестен"},
}

// CatalogueKeys lists every phrase key, so a test can check both languages are
// filled in rather than one of them silently falling back.
func CatalogueKeys() []string {
	out := make([]string, 0, len(catalog))
	for k := range catalog {
		out = append(out, k)
	}
	return out
}
