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
func T(lang Lang, key string) string {
	if m, ok := catalog[key]; ok {
		if s, ok := m[lang]; ok && s != "" {
			return s
		}
		if s, ok := m[LangEN]; ok {
			return s
		}
	}
	return key
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
	"book.asItIsIn":     {LangEN: "as it is in", LangRU: "как есть в"},
	"book.convertedFor": {LangEN: "converted for Kobo — this is what syncs", LangRU: "сконвертировано для Kobo — именно это синкается"},
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
	"flash.sourceNeedsNamePath": {LangEN: "A library needs a name and a folder path.", LangRU: "Источнику нужны имя и путь к папке."},
	"flash.badPath":             {LangEN: "That path could not be resolved.", LangRU: "Не удалось разобрать этот путь."},
	"flash.sourceGone":          {LangEN: "That library no longer exists.", LangRU: "Такого источника больше нет."},
	"flash.suspicious":          {LangEN: "That scan would remove an unusual number of books. If the library really did shrink, use Confirm removal.", LangRU: "Этот скан удалил бы необычно много книг. Если источник действительно уменьшился, нажмите «Подтвердить удаление»."},
	"flash.unreachable":         {LangEN: "The library folder could not be read. Nothing was changed.", LangRU: "Папку источника не удалось прочитать. Ничего не изменено."},
	"flash.sourceOn":            {LangEN: "Library switched on.", LangRU: "Источник включён."},
	"flash.sourceOff":           {LangEN: "Library switched off. Books already on your Kobo stay there.", LangRU: "Источник выключен. Книги, уже загруженные на Kobo, останутся там."},
	"flash.sourceRemoved":       {LangEN: "Library removed. Its books keep their identity, so adding it back changes nothing on your Kobo.", LangRU: "Источник удалён. Книги сохраняют свою идентичность, поэтому повторное добавление ничего не изменит на Kobo."},
	"flash.hidden":              {LangEN: "Hidden. The next sync moves it to your Kobo's archive.", LangRU: "Скрыта. При следующем синке уедет в архив Kobo."},
	"flash.shown":               {LangEN: "Visible again. The next sync sends it back.", LangRU: "Снова видима. Следующий синк отправит её обратно."},
	"flash.converting":          {LangEN: "Converting again in the background.", LangRU: "Конвертируется заново в фоне."},
	"flash.tokenRevoked":        {LangEN: "Token revoked. That reader stops syncing; the others are unaffected.", LangRU: "Ключ отозван. Эта читалка перестала синхронизироваться, остальные не затронуты."},
	"flash.syncReset":           {LangEN: "Sync state cleared. The next sync sends the whole library again. Books you deleted on that reader stay deleted.", LangRU: "Состояние синка сброшено. Следующий синк отправит всю библиотеку заново. Книги, удалённые на этой читалке, останутся удалёнными."},
	"flash.tombstoneGone":       {LangEN: "The next sync sends that book back to the reader.", LangRU: "Следующий синк вернёт эту книгу на читалку."},
	"flash.notYours":            {LangEN: "That belongs to someone else.", LangRU: "Это принадлежит другому человеку."},
	"flash.needNamePassword":    {LangEN: "A person needs a name and a password of at least 8 characters.", LangRU: "Нужны имя и пароль не короче 8 символов."},
	"flash.shortPassword":       {LangEN: "Use a password of at least 8 characters.", LangRU: "Пароль должен быть не короче 8 символов."},
	"flash.passwordChanged":     {LangEN: "Password changed.", LangRU: "Пароль изменён."},
	"flash.cannotRemoveSelf":    {LangEN: "You cannot remove your own account.", LangRU: "Нельзя удалить собственную учётную запись."},
	"flash.lastAdmin":           {LangEN: "That is the only administrator; the server would be unmanageable.", LangRU: "Это единственный администратор — сервером станет некому управлять."},
	"flash.userRemoved":         {LangEN: "Account removed, along with its readers and reading progress.", LangRU: "Учётная запись удалена вместе с её читалками и прогрессом чтения."},

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
