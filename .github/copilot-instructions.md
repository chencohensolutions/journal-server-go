be brief with your response, avoid repeating information

# Project Description

A private photo journal with two components:

- **journal-server-go** (this repo): Go GraphQL server (gqlgen) with mTLS. Users and devices are identified by
  the client certificate; devices enroll with a one-time code. Originals live as plain files in each user's
  `data_folder` with a JSON sidecar next to each (package `media`). See `CLAUDE.md` for the layout and commands.
- **journal-client-kmp** (`../journal-client-kmp`): the Kotlin Multiplatform clients (Android first). Its
  `docs/design.md` is the agreed design for both.
