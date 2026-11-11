//! Which folders the app may hand to git or to an editor: only ones inside
//! the clone root, a watch folder, or a folder the person picked.

use std::path::{Component, Path, PathBuf};

use crate::error::{CmdError, CmdResult};

/// The folder itself, resolved (symlinks followed), when it exists and is
/// inside one of `roots`. Roots that don't exist allow nothing.
pub fn validate(roots: &[PathBuf], path: &str) -> CmdResult<PathBuf> {
    let refused = || {
        CmdError::new("outside", "That folder isn’t one Atelier looks after.")
            .hint("Add it from “Add a repository”, or add its parent as a watch folder.")
    };
    let raw = Path::new(path);
    if path.is_empty() || !raw.is_absolute() {
        return Err(refused());
    }
    let real = raw
        .canonicalize()
        .map_err(|_| CmdError::new("missing", format!("{path} isn’t there any more.")))?;
    if !real.is_dir() {
        return Err(refused());
    }
    let inside = roots
        .iter()
        .filter_map(|root| root.canonicalize().ok())
        .any(|root| real.starts_with(&root));
    if inside {
        Ok(real)
    } else {
        Err(refused())
    }
}

/// One safe path segment for a clone destination (`owner`, `name`).
pub fn safe_segment(segment: &str) -> CmdResult<&str> {
    let ok = !segment.is_empty()
        && segment.len() <= 100
        && !segment.starts_with(['.', '-'])
        && segment
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
        && !segment.ends_with(".git");
    if ok {
        Ok(segment)
    } else {
        Err(CmdError::invalid(format!(
            "“{segment}” can’t be used as a folder name."
        )))
    }
}

/// `owner/name` split into two safe segments.
pub fn split_full_name(full_name: &str) -> CmdResult<(&str, &str)> {
    let (owner, name) = full_name
        .split_once('/')
        .ok_or_else(|| CmdError::invalid(format!("“{full_name}” isn’t a repository name.")))?;
    Ok((safe_segment(owner)?, safe_segment(name)?))
}

/// A path for showing: the home folder becomes `~`.
pub fn display(path: &Path, home: &Path) -> String {
    match path.strip_prefix(home) {
        Ok(rest) if rest.as_os_str().is_empty() => "~".to_string(),
        Ok(rest) => format!("~/{}", rest.display()),
        Err(_) => path.display().to_string(),
    }
}

/// True when `path` has no `..` or other surprises in it.
pub fn is_plain(path: &Path) -> bool {
    path.components().all(|c| {
        matches!(
            c,
            Component::Normal(_) | Component::RootDir | Component::Prefix(_)
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_folders_inside_a_root_pass() {
        let dir = tempfile::tempdir().unwrap();
        let base = dir.path().canonicalize().unwrap();
        let root = base.join("Atelier");
        let picked = base.join("picked");
        let outside = base.join("elsewhere/repo");
        for d in [&root.join("me/app"), &picked, &outside] {
            std::fs::create_dir_all(d).unwrap();
        }
        std::fs::write(root.join("file.txt"), "x").unwrap();
        let roots = vec![root.clone(), picked.clone(), base.join("not-there")];

        let ok = validate(&roots, root.join("me/app").to_str().unwrap()).unwrap();
        assert_eq!(ok, root.join("me/app"));
        assert!(validate(&roots, picked.to_str().unwrap()).is_ok());

        assert_eq!(
            validate(&roots, outside.to_str().unwrap())
                .unwrap_err()
                .kind,
            "outside"
        );
        // `..` can't climb out.
        let sneaky = format!("{}/me/../../elsewhere/repo", root.display());
        assert_eq!(validate(&roots, &sneaky).unwrap_err().kind, "outside");
        // Relative, empty, a file, a missing folder.
        assert_eq!(validate(&roots, "me/app").unwrap_err().kind, "outside");
        assert_eq!(validate(&roots, "").unwrap_err().kind, "outside");
        assert_eq!(
            validate(&roots, root.join("file.txt").to_str().unwrap())
                .unwrap_err()
                .kind,
            "outside"
        );
        assert_eq!(
            validate(&roots, root.join("gone").to_str().unwrap())
                .unwrap_err()
                .kind,
            "missing"
        );
        // A sibling whose name merely starts like a root.
        let twin = base.join("Atelier-other");
        std::fs::create_dir_all(&twin).unwrap();
        assert_eq!(
            validate(&roots, twin.to_str().unwrap()).unwrap_err().kind,
            "outside"
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_symlink_out_of_a_root_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let base = dir.path().canonicalize().unwrap();
        let root = base.join("root");
        let outside = base.join("outside");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&outside).unwrap();
        std::os::unix::fs::symlink(&outside, root.join("link")).unwrap();
        let err = validate(
            std::slice::from_ref(&root),
            root.join("link").to_str().unwrap(),
        )
        .unwrap_err();
        assert_eq!(err.kind, "outside");
    }

    #[test]
    fn names_and_display() {
        assert_eq!(
            split_full_name("DemiInfinity/Atelier").unwrap(),
            ("DemiInfinity", "Atelier")
        );
        assert!(split_full_name("a/b/c").is_err());
        assert!(split_full_name("../x").is_err());
        assert!(split_full_name("a/.git").is_err());
        assert!(split_full_name("a/-rf").is_err());
        assert!(split_full_name("noslash").is_err());
        let home = Path::new("/Users/x");
        assert_eq!(
            display(Path::new("/Users/x/Atelier/a"), home),
            "~/Atelier/a"
        );
        assert_eq!(display(Path::new("/Users/x"), home), "~");
        assert_eq!(display(Path::new("/opt/y"), home), "/opt/y");
        assert!(is_plain(Path::new("/a/b")));
        assert!(!is_plain(Path::new("/a/../b")));
    }
}
