# Fonts (optional)

Run `scripts/fetch-fonts.sh` to download:

- **Anton-Regular.ttf** — SIL OFL 1.1 — the free Impact-alike used for
  classic meme captions (`OFL-Anton.txt` downloaded alongside).
- **Oswald-SemiBold.woff2** — SIL OFL 1.1 — for the caption-bar style
  (`OFL-Oswald.txt` downloaded alongside).

Without them, the caption font stack falls back to your locally installed
Impact / Arial Black — the app remains fully functional. Impact itself is
deliberately never bundled: it is not freely redistributable (using your
system's copy to render output is fine; shipping the .ttf is not).
