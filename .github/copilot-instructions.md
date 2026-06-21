be brief with your response, avoid repeating information

# Project Description

A journal app with two components:

- **journal-server-go**: Go GraphQL server (gqlgen) with mTLS. Identifies users by matching the client certificate CN against `config.json`.
- **journal-client-android**: Android client that authenticates using a certificate from the system KeyChain.


