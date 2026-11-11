//! Terminal output: the studio's colours, and small text helpers.

use std::io::{IsTerminal, Write};

use anstyle::{AnsiColor, Style};

/// Writes a line to standard output. A closed pipe (`atl … | head`) is not
/// an error worth a panic.
macro_rules! outln {
    () => { $crate::ui::write_out("\n") };
    ($($arg:tt)*) => { $crate::ui::write_out(&format!("{}\n", format_args!($($arg)*))) };
}
/// Like `outln!`, without the line break (for "Creating… done").
macro_rules! out {
    ($($arg:tt)*) => { $crate::ui::write_out(&format!($($arg)*)) };
}
pub(crate) use {out, outln};

pub fn write_out(text: &str) {
    let mut stdout = std::io::stdout().lock();
    let _ = stdout.write_all(text.as_bytes());
    let _ = stdout.flush();
}

/// The palette: amber is yours, blue is GitHub, violet is agents and
/// Connect, green is done, red needs attention.
#[derive(Clone, Copy, Debug)]
pub struct Ui {
    pub color: bool,
}

impl Ui {
    /// Colour only on a terminal, and never with `NO_COLOR` set.
    pub fn detect() -> Ui {
        let no_color = std::env::var_os("NO_COLOR").is_some_and(|v| !v.is_empty());
        let dumb = std::env::var("TERM").is_ok_and(|t| t == "dumb");
        Ui {
            color: std::io::stdout().is_terminal() && !no_color && !dumb,
        }
    }

    fn paint(&self, style: Style, text: &str) -> String {
        if self.color && !text.is_empty() {
            format!("{style}{text}{style:#}")
        } else {
            text.to_string()
        }
    }

    fn fg(color: AnsiColor) -> Style {
        Style::new().fg_color(Some(color.into()))
    }

    pub fn amber(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Yellow), text)
    }
    pub fn amber_bold(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Yellow).bold(), text)
    }
    pub fn blue(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Blue), text)
    }
    pub fn blue_bold(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Blue).bold(), text)
    }
    pub fn violet(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Magenta), text)
    }
    pub fn violet_bold(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Magenta).bold(), text)
    }
    pub fn green(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Green), text)
    }
    pub fn green_bold(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Green).bold(), text)
    }
    pub fn red(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Red), text)
    }
    pub fn red_bold(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::Red).bold(), text)
    }
    pub fn muted(&self, text: &str) -> String {
        self.paint(Self::fg(AnsiColor::BrightBlack), text)
    }
    pub fn bold(&self, text: &str) -> String {
        self.paint(Style::new().bold(), text)
    }
}

/// Pads to `width` characters (never cuts).
pub fn pad(text: &str, width: usize) -> String {
    let length = text.chars().count();
    format!("{text}{}", " ".repeat(width.saturating_sub(length)))
}

/// Cuts to at most `width` characters, ending in `…` when cut.
pub fn truncate(text: &str, width: usize) -> String {
    if text.chars().count() <= width {
        return text.to_string();
    }
    let mut cut: String = text.chars().take(width.saturating_sub(1)).collect();
    cut.truncate(cut.trim_end().len());
    cut.push('…');
    cut
}

/// The widest of `items`, at most `cap`.
pub fn column<'a>(items: impl IntoIterator<Item = &'a str>, cap: usize) -> usize {
    items
        .into_iter()
        .map(|s| s.chars().count())
        .max()
        .unwrap_or(0)
        .min(cap)
}

/// `1 commit`, `2 commits`.
pub fn count(n: u64, one: &str, many: &str) -> String {
    format!("{n} {}", if n == 1 { one } else { many })
}

/// `22s`, `1m 48s`, `2h 05m`.
pub fn duration(seconds: u64) -> String {
    match seconds {
        s if s < 60 => format!("{s}s"),
        s if s < 3600 => format!("{}m {:02}s", s / 60, s % 60),
        s => format!("{}h {:02}m", s / 3600, (s % 3600) / 60),
    }
}

/// `DemiInfinity/Atelier` → `Atelier`.
pub fn short_repo(full_name: &str) -> &str {
    full_name.rsplit('/').next().unwrap_or(full_name)
}

/// How long ago an RFC 3339 time was: `just now`, `2 min`, `3 h`, `4 d`.
/// Anything that isn't a time is given back as it is.
pub fn since(at: &str, now: time::OffsetDateTime) -> String {
    let Ok(then) = time::OffsetDateTime::parse(at, &time::format_description::well_known::Rfc3339)
    else {
        return at.to_string();
    };
    let seconds = (now - then).whole_seconds().max(0);
    match seconds {
        s if s < 60 => "just now".to_string(),
        s if s < 3600 => format!("{} min", s / 60),
        s if s < 86_400 => format!("{} h", s / 3600),
        s => format!("{} d", s / 86_400),
    }
}

/// What to call this machine in a sentence.
pub fn this_machine() -> &'static str {
    match std::env::consts::OS {
        "macos" => "this Mac",
        "windows" => "this PC",
        _ => "this machine",
    }
}

/// Asks a yes/no question on the terminal. Anything but y/yes is no.
pub fn confirm(question: &str, ui: &Ui) -> bool {
    out!("{question} {}", ui.muted("[y/N] "));
    let mut answer = String::new();
    if std::io::stdin().read_line(&mut answer).is_err() {
        return false;
    }
    matches!(answer.trim().to_lowercase().as_str(), "y" | "yes")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn text_helpers() {
        assert_eq!(pad("ab", 4), "ab  ");
        assert_eq!(pad("abcdef", 4), "abcdef");
        assert_eq!(pad("ci · test", 11), "ci · test  ");
        assert_eq!(truncate("short", 10), "short");
        assert_eq!(truncate("a long title here", 8), "a long…");
        assert_eq!(column(["a", "abc", "ab"], 10), 3);
        assert_eq!(column(["abcdef"], 4), 4);
        assert_eq!(count(1, "commit", "commits"), "1 commit");
        assert_eq!(count(42, "commit", "commits"), "42 commits");
        assert_eq!(duration(22), "22s");
        assert_eq!(duration(108), "1m 48s");
        assert_eq!(duration(64), "1m 04s");
        assert_eq!(duration(7500), "2h 05m");
        assert_eq!(short_repo("DemiInfinity/Atelier"), "Atelier");
        assert_eq!(short_repo("Atelier"), "Atelier");
    }

    #[test]
    fn since_is_humane() {
        let now = time::macros::datetime!(2026-11-11 09:16:02 UTC);
        assert_eq!(since("2026-11-11T09:14:02Z", now), "2 min");
        assert_eq!(since("2026-11-11T09:15:40Z", now), "just now");
        assert_eq!(since("2026-11-11T06:00:00Z", now), "3 h");
        assert_eq!(since("2026-11-07T09:00:00+01:00", now), "4 d");
        assert_eq!(since("…", now), "…");
    }

    #[test]
    fn colour_is_only_added_when_on() {
        let off = Ui { color: false };
        assert_eq!(off.amber("x"), "x");
        assert_eq!(off.muted("x"), "x");
        let on = Ui { color: true };
        assert_eq!(on.amber("x"), "\u{1b}[33mx\u{1b}[0m");
        assert_eq!(on.violet_bold("x"), "\u{1b}[1m\u{1b}[35mx\u{1b}[0m");
        assert_eq!(on.red(""), "");
    }
}
