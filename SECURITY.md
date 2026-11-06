# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for security problems. Use GitHub's private reporting
instead: open this repository's **Security** tab and choose **Report a vulnerability**.

Include what you found, how to reproduce it, and the impact you expect. You'll get an
acknowledgement within a few days.

## Supported versions

Only the latest commit on `main` is supported.

## Data in this repository

All data in this repository is **synthetic**. The demo services (`checkout-api`,
`search-api`, `notifications-worker`, `billing-api`) belong to a fictional company, and their
traffic is generated from a fixed random seed by `burnrate seed`. No real people, customers
or companies are represented.

The repository contains no credentials. API tokens are generated on your machine, shown once
in your terminal, and stored only as SHA-256 hashes in your local, gitignored database.
