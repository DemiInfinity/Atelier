//! This device's SSH key: use an ed25519 key that is already there, or make
//! a dedicated one with `ssh-keygen`, and register it with the studio.

use std::path::{Path, PathBuf};
use std::process::Stdio;

use crate::api::SshKey;
use crate::client::Client;
use crate::error::{Error, Result};

/// The dedicated key's file name inside the SSH folder.
pub const DEVICE_KEY_NAME: &str = "atelier_device_ed25519";

/// Public keys looked for, in order.
const CANDIDATES: [&str; 2] = ["id_ed25519.pub", "atelier_device_ed25519.pub"];

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PublicKey {
    /// The `.pub` file.
    pub path: PathBuf,
    /// `ssh-ed25519 AAAA… comment`.
    pub text: String,
}

impl PublicKey {
    /// The private key next to it.
    pub fn private_path(&self) -> PathBuf {
        self.path.with_extension("")
    }

    /// True for the key this crate generates, which ssh doesn't offer by
    /// itself (see [`crate::Git::with_ssh_identity`]).
    pub fn is_dedicated(&self) -> bool {
        self.path.file_stem().and_then(|s| s.to_str()) == Some(DEVICE_KEY_NAME)
    }
}

fn read_public_key(path: &Path) -> Option<PublicKey> {
    let text = std::fs::read_to_string(path).ok()?;
    let text = text.lines().next()?.trim().to_string();
    text.starts_with("ssh-ed25519 ").then(|| PublicKey {
        path: path.to_path_buf(),
        text,
    })
}

/// An ed25519 public key already in `ssh_dir`: `id_ed25519.pub`, else the
/// dedicated device key.
pub fn find_public_key(ssh_dir: &Path) -> Option<PublicKey> {
    CANDIDATES
        .iter()
        .find_map(|name| read_public_key(&ssh_dir.join(name)))
}

/// The dedicated device key's private half, when it exists.
pub fn dedicated_identity(ssh_dir: &Path) -> Option<PathBuf> {
    let private = ssh_dir.join(DEVICE_KEY_NAME);
    (private.is_file() && find_public_key(ssh_dir).is_some_and(|k| k.is_dedicated()))
        .then_some(private)
}

/// Finds a key or generates `<ssh_dir>/atelier_device_ed25519` (no
/// passphrase, comment `comment`) with `ssh-keygen`. The flag says whether
/// a new key was generated.
pub async fn ensure_public_key(ssh_dir: &Path, comment: &str) -> Result<(PublicKey, bool)> {
    if let Some(key) = find_public_key(ssh_dir) {
        return Ok((key, false));
    }
    std::fs::create_dir_all(ssh_dir)
        .map_err(|e| Error::io(format!("Couldn’t create {}", ssh_dir.display()), e))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(ssh_dir, std::fs::Permissions::from_mode(0o700));
    }
    let private = ssh_dir.join(DEVICE_KEY_NAME);
    if private.exists() {
        return Err(Error::Ssh(format!(
            "{} exists but its .pub file is missing or isn’t an ed25519 key.",
            private.display()
        )));
    }
    let output = tokio::process::Command::new("ssh-keygen")
        .args(["-q", "-t", "ed25519", "-N", "", "-C", comment, "-f"])
        .arg(&private)
        .stdin(Stdio::null())
        .output()
        .await
        .map_err(|e| match e.kind() {
            std::io::ErrorKind::NotFound => Error::MissingProgram("ssh-keygen".into()),
            _ => Error::io("Couldn’t run ssh-keygen", e),
        })?;
    if !output.status.success() {
        return Err(Error::Ssh(format!(
            "ssh-keygen couldn’t make a key: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        )));
    }
    let key = read_public_key(&private.with_extension("pub"))
        .ok_or_else(|| Error::Ssh("ssh-keygen didn’t leave a public key.".into()))?;
    Ok((key, true))
}

/// What [`register_device_key`] did.
#[derive(Clone, Debug, PartialEq)]
pub struct RegisteredKey {
    pub key: PublicKey,
    /// A new key pair was generated on this machine.
    pub generated: bool,
    /// The studio's answer (`created` is false when it knew the key already).
    pub registered: SshKey,
}

/// Finds or generates this device's key and registers it with
/// `POST /device/ssh-key` under `title` (e.g. `MacBook (atl)`).
pub async fn register_device_key(
    client: &Client,
    ssh_dir: &Path,
    title: &str,
) -> Result<RegisteredKey> {
    let (key, generated) = ensure_public_key(ssh_dir, title).await?;
    let registered = client.register_ssh_key(title, &key.text).await?;
    Ok(RegisteredKey {
        key,
        generated,
        registered,
    })
}
