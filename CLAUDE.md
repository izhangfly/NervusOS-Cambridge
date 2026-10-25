# Working in this repository

## Where work goes

- github.com/izhangfly/NervusOS-Cambridge is the only repository NervusOS work is pushed to, from any machine
  (the Mac, the Dell, the server). Do not push to izhangfly/piper-act, the Nervus-OS mirrors or any other
  repository: anything new there belongs here.
- Commit and push after every edit: `scripts/push.sh "Subject" ["Body"]`. It commits nothing unless
  `scripts/secret-scan.py` passes, and it pushes nowhere but this repository.
- The Mac working copy is `~/NervusOS-Cambridge`; the Dell's is `~/NervusOS-Cambridge` too. Pull before you
  edit on either one.

## What must never be committed

- The relay server's address or hostname, in any file, doc or comment. Write `YOUR_SERVER_IP` instead.
- Keys: `keys/`, `**/signing/*.pem`, SSH or deploy keys, Syncthing API keys, tokens, passwords.
- Chinese text. The repository is English only.

## Safety rules for the robot host (Surface Pro 9, A7-BAH)

- Hosted mode only: never change Ubuntu's GDM or default target, never remove Ubuntu.
- The PiPER driver (127.0.0.1:8770) and the ACT engine (127.0.0.1:8795) listen on loopback only and must stay
  that way: neither has authentication and both move the arm.
- Never move the arm when testing (owner's rule): use the simulators, fake drivers, dry runs and read-only
  state. Real motion only when the owner starts it. Leave the arm enabled when you finish: it goes
  limp when disabled.
