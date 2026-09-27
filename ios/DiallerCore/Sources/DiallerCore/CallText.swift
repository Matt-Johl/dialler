import Foundation

/// The words the app's lists put beside a call or a contact: "Ext 118",
/// "2m 14s", "Yesterday", "AN". Kept here rather than in the views because
/// the app target has no tests (the same reason `CallProgress` owns its
/// labels).
public enum CallText {
    /// A short all-digit number is an extension and reads "Ext 118";
    /// anything else (a full number, a SIP user name) is shown as it is.
    public static func party(_ number: String) -> String {
        isExtension(number) ? "Ext \(number)" : number
    }

    /// One to six digits: an internal extension rather than a phone number.
    public static func isExtension(_ number: String) -> Bool {
        (1...6).contains(number.count) && number.allSatisfy(\.isASCIIDigit)
    }

    /// A conversation's length for a list row: "48s", "2m 14s", "1h 3m".
    public static func duration(_ seconds: TimeInterval) -> String {
        let s = max(0, Int(seconds.rounded()))
        if s < 60 { return "\(s)s" }
        if s < 3600 { return "\(s / 60)m \(s % 60)s" }
        return "\(s / 3600)h \(s / 60 % 60)m"
    }

    /// When a call happened, as Phone's Recents says it: the time today,
    /// "Yesterday", the weekday within the last week, else the date.
    public static func when(_ date: Date, now: Date = Date(), calendar: Calendar = .current, locale: Locale = .current) -> String {
        var cal = calendar
        cal.locale = locale
        if cal.isDate(date, inSameDayAs: now) {
            return date.formatted(Date.FormatStyle(date: .omitted, time: .shortened, locale: locale, calendar: cal, timeZone: cal.timeZone))
        }
        if let yesterday = cal.date(byAdding: .day, value: -1, to: now), cal.isDate(date, inSameDayAs: yesterday) {
            return "Yesterday"
        }
        let startOfToday = cal.startOfDay(for: now)
        if let weekAgo = cal.date(byAdding: .day, value: -6, to: startOfToday), date >= weekAgo {
            return date.formatted(Date.FormatStyle(locale: locale, calendar: cal, timeZone: cal.timeZone).weekday(.wide))
        }
        return date.formatted(Date.FormatStyle(date: .numeric, time: .omitted, locale: locale, calendar: cal, timeZone: cal.timeZone))
    }

    /// Up to two initials for a name ("Amara Nwosu" → "AN", "Facilities"
    /// → "F"); nil when the name has no letters, i.e. is a number.
    public static func initials(_ name: String) -> String? {
        let words = name.split(whereSeparator: { $0.isWhitespace || $0 == "-" })
        let letters = words.compactMap { $0.first(where: \.isLetter) }
        guard let first = letters.first else { return nil }
        let pair = letters.count > 1 ? [first, letters[letters.count - 1]] : [first]
        return String(pair).uppercased()
    }

    /// A Recents row's second line: who (unless the first line already is
    /// the number) and then how long, or why there was no conversation.
    /// "Ext 118 · 2m 14s", "Ext 100 · Missed", "Busy".
    public static func detail(for record: CallRecord, name: String) -> String {
        let what = record.duration.map(duration) ?? record.outcome.label
        let number = record.number
        guard !number.isEmpty, name != number else { return what }
        return "\(party(number)) · \(what)"
    }
}

private extension Character {
    var isASCIIDigit: Bool { ("0"..."9").contains(self) }
}
