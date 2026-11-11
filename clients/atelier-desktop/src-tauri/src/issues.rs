//! A repository's issues: the list, one with its conversation, opening a
//! new one, commenting, closing and reopening.
//!
//! Everything the studio sends (titles, bodies, comments, label names) is
//! untrusted. Titles and label names are passed on as plain text. Bodies and
//! comments are Markdown: they go along as written and as the typed tree of
//! [`crate::markdown`], which the window draws without ever making markup
//! out of content.

use std::time::Duration;

use atelier_core::api::{Issue, IssueSummary, IssuesQuery, Label, PullComment};
use serde::Serialize;

use crate::desk::Desk;
use crate::error::{CmdError, CmdResult};
use crate::markdown::{self, Block};
use crate::paths;
use crate::pulls::{author_label, comment_row, via_badge, Badge};

const PER_PAGE: u32 = 30;
/// The studio's own limits are not known here; these only stop accidents.
const MAX_TITLE: usize = 256;
const MAX_BODY: usize = 65_536;

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct LabelView {
    pub name: String,
    /// `#rrggbb`, or nothing when the studio sent no usable colour.
    pub color: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct IssueRow {
    pub number: u64,
    pub title: String,
    pub open: bool,
    pub by: String,
    /// `via Tether` (blue) for an issue that came from GitHub.
    pub via: Option<Badge>,
    pub labels: Vec<LabelView>,
    pub comments: u64,
    pub created_at: String,
    pub updated_at: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct IssuesView {
    pub rows: Vec<IssueRow>,
    pub has_more: bool,
    pub page: u32,
}

#[derive(Clone, Debug, Serialize)]
pub struct IssueCommentRow {
    pub who: String,
    /// `bot · via Tether`.
    pub tag: String,
    /// `yours`, `github`, `agents` or `connect`.
    pub tone: &'static str,
    pub at: String,
    pub body: String,
    /// The body as Markdown, parsed.
    pub body_md: Vec<Block>,
}

#[derive(Clone, Debug, Serialize)]
pub struct IssueView {
    pub repo: String,
    pub number: u64,
    pub title: String,
    pub open: bool,
    pub by: String,
    pub via: Option<Badge>,
    pub labels: Vec<LabelView>,
    pub body: String,
    /// The body as Markdown, parsed.
    pub body_md: Vec<Block>,
    pub comments: Vec<IssueCommentRow>,
    pub created_at: String,
    pub web_path: String,
}

/// `d73a4a`, `#D73A4A` or `#abc` → `#d73a4a` / `#abc`; anything else is
/// dropped, so nothing but a colour ever reaches the page's styles.
pub fn safe_color(color: &str) -> Option<String> {
    let hex = color.trim().trim_start_matches('#');
    (matches!(hex.len(), 3 | 6) && hex.chars().all(|c| c.is_ascii_hexdigit()))
        .then(|| format!("#{}", hex.to_ascii_lowercase()))
}

fn labels(labels: &[Label]) -> Vec<LabelView> {
    labels
        .iter()
        .filter(|l| !l.name.is_empty())
        .map(|l| LabelView {
            name: l.name.clone(),
            color: safe_color(&l.color),
        })
        .collect()
}

pub fn issue_row(issue: &IssueSummary, login: Option<&str>) -> IssueRow {
    IssueRow {
        number: issue.number,
        title: issue.title.clone(),
        open: issue.state != "closed",
        by: author_label(&issue.author, login),
        via: via_badge(issue.via),
        labels: labels(&issue.labels),
        comments: issue.comments,
        created_at: issue.created_at.clone(),
        updated_at: issue.updated_at.clone(),
    }
}

pub fn issue_view(repo: &str, number: u64, issue: &Issue, login: Option<&str>) -> IssueView {
    IssueView {
        repo: repo.to_string(),
        number,
        title: issue.title.clone(),
        open: issue.state != "closed",
        by: author_label(&issue.author, login),
        via: via_badge(issue.via),
        labels: labels(&issue.labels),
        body: issue.body.clone(),
        body_md: markdown::render(&issue.body),
        comments: issue
            .comments
            .iter()
            .map(|c| {
                // The same words and colours as a pull request's comments.
                let row = comment_row(
                    &PullComment {
                        author: c.author.clone(),
                        via: c.via,
                        kind: c.kind.clone(),
                        body: String::new(),
                        at: String::new(),
                    },
                    login,
                );
                IssueCommentRow {
                    who: row.who,
                    tag: row.tag,
                    tone: row.tone,
                    at: c.created_at.clone(),
                    body: c.body.clone(),
                    body_md: markdown::render(&c.body),
                }
            })
            .collect(),
        created_at: issue.created_at.clone(),
        web_path: format!("/{repo}/issues/{number}"),
    }
}

fn parse_state(state: &str) -> CmdResult<&'static str> {
    match state {
        "open" => Ok("open"),
        "closed" => Ok("closed"),
        "all" => Ok("all"),
        other => Err(CmdError::invalid(format!(
            "“{other}” isn’t a kind of issue list."
        ))),
    }
}

impl Desk {
    fn login_name(&self) -> Option<String> {
        self.me_cached().map(|me| me.person.login)
    }

    /// One page of a repository's issues (`state`: `open`, `closed`, `all`).
    pub async fn issues_list(&self, repo: &str, state: &str, page: u32) -> CmdResult<IssuesView> {
        paths::split_full_name(repo)?;
        let state = parse_state(state)?;
        let page = page.max(1);
        let list = self.watch(
            self.client()?
                .issues(&IssuesQuery {
                    repo: repo.to_string(),
                    state: state.to_string(),
                    page: Some(page),
                    per_page: Some(PER_PAGE),
                })
                .await,
        )?;
        let _ = self.me(Duration::from_secs(300)).await;
        let login = self.login_name();
        Ok(IssuesView {
            rows: list
                .issues
                .iter()
                // A studio that ignored the filters is narrowed here.
                .filter(|i| i.repo.is_empty() || i.repo.eq_ignore_ascii_case(repo))
                .filter(|i| state == "all" || (i.state != "closed") == (state == "open"))
                .map(|i| issue_row(i, login.as_deref()))
                .collect(),
            has_more: list.has_more,
            page,
        })
    }

    pub async fn issue_detail(&self, repo: &str, number: u64) -> CmdResult<IssueView> {
        let (owner, name) = paths::split_full_name(repo)?;
        let issue = self.watch(self.client()?.issue(owner, name, number).await)?;
        let _ = self.me(Duration::from_secs(300)).await;
        Ok(issue_view(
            repo,
            number,
            &issue,
            self.login_name().as_deref(),
        ))
    }

    /// Opens an issue. Returns its number when the studio says it.
    pub async fn issue_create(
        &self,
        repo: &str,
        title: &str,
        body: &str,
    ) -> CmdResult<Option<u64>> {
        let (owner, name) = paths::split_full_name(repo)?;
        let title = title.trim();
        if title.is_empty() {
            return Err(CmdError::invalid("Give the issue a title."));
        }
        if title.chars().count() > MAX_TITLE || body.len() > MAX_BODY {
            return Err(CmdError::invalid("That is too long for an issue."));
        }
        let made = self.watch(
            self.client()?
                .create_issue(owner, name, title, body.trim())
                .await,
        )?;
        Ok((made.number > 0).then_some(made.number))
    }

    pub async fn issue_comment(&self, repo: &str, number: u64, body: &str) -> CmdResult<()> {
        let (owner, name) = paths::split_full_name(repo)?;
        let body = body.trim();
        if body.is_empty() {
            return Err(CmdError::invalid("Write something first."));
        }
        if body.len() > MAX_BODY {
            return Err(CmdError::invalid("That is too long for a comment."));
        }
        self.watch(
            self.client()?
                .comment_on_issue(owner, name, number, body)
                .await,
        )
    }

    /// Closes an issue, or opens it again.
    pub async fn issue_set_open(&self, repo: &str, number: u64, open: bool) -> CmdResult<()> {
        let (owner, name) = paths::split_full_name(repo)?;
        self.watch(
            self.client()?
                .set_issue_open(owner, name, number, open)
                .await,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_colours_get_through() {
        assert_eq!(safe_color("#D73A4A").as_deref(), Some("#d73a4a"));
        assert_eq!(safe_color("d73a4a").as_deref(), Some("#d73a4a"));
        assert_eq!(safe_color("#abc").as_deref(), Some("#abc"));
        for bad in [
            "",
            "red",
            "#12345",
            "#d73a4a; background: url(x)",
            "var(--x)",
            "#gggggg",
        ] {
            assert_eq!(safe_color(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn rows_say_who_and_whether_it_is_open() {
        let issue = IssueSummary {
            number: 7,
            title: "<b>Blurry</b>".into(),
            state: "closed".into(),
            author: "owner:DemiInfinity".into(),
            labels: vec![
                Label {
                    name: "bug".into(),
                    color: "d73a4a".into(),
                },
                Label {
                    name: String::new(),
                    color: "fff".into(),
                },
                Label {
                    name: "odd".into(),
                    color: "javascript:1".into(),
                },
            ],
            comments: 2,
            ..IssueSummary::default()
        };
        let row = issue_row(&issue, Some("demiinfinity"));
        assert!(!row.open);
        assert_eq!(row.by, "you");
        assert_eq!(row.title, "<b>Blurry</b>", "text stays text");
        assert_eq!(
            row.labels,
            [
                LabelView {
                    name: "bug".into(),
                    color: Some("#d73a4a".into())
                },
                LabelView {
                    name: "odd".into(),
                    color: None
                }
            ]
        );
        assert!(parse_state("merged").is_err());
    }
}
