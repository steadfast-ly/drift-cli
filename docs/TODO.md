# drift-cli — TODO

No open items. Prior items closed in P6 S5:

- **Item 1** (`auth status` whoami): closed. `auth status` now calls
  `GET /api/v1/auth/whoami` and renders owner, role, channel, label, scopes
  and expiry. The false disclaimer was removed. A warning fires when the
  credential expires within 24 hours.

- **Item 2** (`doctor` server version): CLI side closed. The server-side fix
  landed in P6 S1 (`DRIFT_VERSION` from the Helm chart via `stamp-chart.sh`);
  the CLI already reads whatever the server advertises, so no CLI change was
  needed.
