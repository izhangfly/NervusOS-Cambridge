# server/: what runs on the relay server

One small Ubuntu server (Hetzner) carries the Mac-Dell link when the two machines are not on the same network.
Nothing here trains or runs a policy: training happens on the Mac, the arm and cameras are on the Dell, and the
server only relays bytes. Its address is not written anywhere in this repository; replace `YOUR_SERVER_IP`.

| Service | Port | What it does |
|---|---|---|
| `syncthing-relaysrv` (strelaysrv) | 22067/tcp (status on 22070) | A private Syncthing relay. When the Mac and the Dell cannot reach each other directly (different networks, NAT on both sides), Syncthing sends the shared folder through it. `-pools=` keeps it out of the public relay pool, so only devices configured to use it do. |
| `syncthing-discosrv` (stdiscosrv) | 8444/tcp | A private global discovery server: each device publishes where it can currently be reached, so the other finds it after a roam without the public discovery servers. |
| `act-portal` | 8850/tcp | The read-only training dashboard from the Piper-ACT project (`legacy/piper-act/portal`). |

The ACT pipeline uses the first two without knowing it: jobs, datasets and policies travel through the shared
folder (`~/Shared/NervusACT`), and Syncthing picks the direct LAN path when there is one and this relay when
there isn't. ACT Studio's System tab shows which path is in use (`act/engine/nervus_act/link.py`).

## Setting it up again

```bash
sudo apt install syncthing-relaysrv syncthing-discosrv
sudo cp syncthing-relaysrv.default /etc/default/syncthing-relaysrv
sudo cp syncthing-discosrv.default /etc/default/syncthing-discosrv
sudo systemctl enable --now syncthing-relaysrv syncthing-discosrv
sudo ufw allow 22067/tcp && sudo ufw allow 8444/tcp
```

Then on the Mac and on the Dell, in Syncthing's settings:

- **Sync Protocol Listen Addresses**: `default, relay://YOUR_SERVER_IP:22067/?id=RELAY_DEVICE_ID`
  (the relay prints its device ID in its log on first start).
- **Global Discovery Servers**: `https://YOUR_SERVER_IP:8444/?id=DISCOVERY_DEVICE_ID`, plus `default` if you
  also want the public ones.

## Hardening notes

- The relay's status page (22070) is public; firewall it if you don't use it.
- strelaysrv's `-token` option would stop anyone who learns the address from relaying through it.
- `legacy/piper-act/server/relay.py` (port 8790) belonged to the Windows-era Dell portal and forwards an
  unauthenticated arm-control page. It should not be left running; nothing in the current system uses it.
