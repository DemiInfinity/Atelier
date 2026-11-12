# The Atelier badge

The badge is a small file called `.atelier` in your GitHub **profile repository** (the one named after you, `<your-login>/<your-login>`, the same repository that holds your profile README). Anyone running Atelier who comes across you on GitHub sees a small mark next to your name.

It is opt-in, public, and you can delete the file at any time.

## Waiting for Atelier

Atelier isn't ready for everyone yet. If you'd like to show you're waiting for it, add a file named `.atelier` to your profile repository with this in it:

```json
{
  "waiting": true
}
```

That's all. People running Atelier will see a quiet "Waiting for Atelier" mark on your profile. There is a copy to download in [`examples/waiting.atelier`](examples/waiting.atelier); rename it to `.atelier`.

No profile repository yet? Create a public repository on GitHub with exactly your username as its name, and put the file at the top level.

## I use Atelier

Once you run your own studio, Atelier writes the file for you: **Settings → Tether → Badge**. It looks like this:

```json
{
  "atelier": true,
  "studio": "https://git.example.com/you"
}
```

`studio` is only filled in when your studio has a public address; a home-network address is never published. If both `atelier` and `waiting` are true, `atelier` wins.

## What the file does and doesn't do

- It is read by other people's Atelier studios, through GitHub, about once a day.
- It sends nothing anywhere. There is no list of badges and no tracking; a studio only looks at the profiles its owner happens to view.
- Deleting the file removes the mark within a day.
