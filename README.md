# CasaNova 🦁

### Your server. Your space. Your rules.
<img width="1919" height="1002" alt="imagen" src="https://github.com/user-attachments/assets/7caf2204-4be6-4474-a071-0b046492d3f1" />

**A new home for everything you self-host.**
<img width="1919" height="996" alt="imagen" src="https://github.com/user-attachments/assets/86e5a2f6-ba09-4738-b512-fb70f64c3645" />

CasaNova is an open-source home server platform built on the foundations of [CasaOS](https://github.com/IceWhaleTech/CasaOS) and inspired by the possibilities of [ZimaOS](https://github.com/IceWhaleTech/ZimaOS).

It started with a simple idea: **what if the home server experience we already love could go a little further?**

More control. More reliability. A modern interface. And fewer reasons to open a terminal just to keep everything running.

CasaNova brings your applications, storage, files, and server management together in one familiar place, while reworking the foundations to make everyday self-hosting a little more comfortable.

Whether you're running a small media server, experimenting with containers, or managing a growing collection of self-hosted services, CasaNova is being built to feel like home.

**Welcome to CasaNova. Make yourself at home.** 🏡

---

## ✨ A familiar experience, with new possibilities

*Screenshot coming soon — CasaNova dashboard*

CasaNova doesn't try to reinvent everything that made CasaOS special.

Instead, it builds upon that experience with a refreshed interface, improvements beneath the surface, and a focus on making your server easier to manage as it grows.

### 🖥️ A refreshed dashboard
<img width="1917" height="990" alt="imagen" src="https://github.com/user-attachments/assets/2014a004-e849-4b5b-8e29-d42551777ec2" />

A modern Vue 3 interface powered by Pinia and PrimeVue, designed to bring your server's essential controls into one place.

Manage your environment from the browser, without making everyday tasks more complicated than they need to be.

### 📦 Your applications, your way
<img width="1917" height="1001" alt="imagen" src="https://github.com/user-attachments/assets/5cbd6650-2d17-4740-97e9-b99d83fd5806" />

Install and manage Docker applications and Compose stacks from a familiar interface.

CasaNova retains compatibility with CasaOS application metadata, including the `x-casaos` extension, and supports configurable application catalogs.

The CasaOS community App Store remains available by default, so you don't have to leave your favorite applications behind.

### 💾 Storage without the guesswork
<img width="430" height="913" alt="imagen" src="https://github.com/user-attachments/assets/1479e240-f4e3-4f82-9dee-4d4711b99857" />

Keep an eye on your disks, mounts, and storage configuration.

CasaNova includes storage management capabilities covering SMART information, mergerfs, and mounted devices, with additional tools for managing your server's files.

### 🔄 Updates without the terminal

Updating your server shouldn't always require an SSH session.

CasaNova supports installing release packages directly through the browser, as well as discovering packages already stored on the server or attached storage devices.

Upload a compatible update package, validate it, and let the update system handle the installation.

### 🛡️ Reliability matters
<img width="1904" height="795" alt="imagen" src="https://github.com/user-attachments/assets/40efc4e7-fa73-470a-861e-bc96f1dc3967" />

A home server is supposed to be there when you need it.

Behind the interface, CasaNova introduces improvements to service recovery, request timeouts, configuration persistence, and gateway protection.

These aren't the most glamorous features, but they're part of what makes a platform dependable.

### 🔐 Your server stays yours
<img width="1919" height="1002" alt="imagen" src="https://github.com/user-attachments/assets/e1466381-e0c6-40fc-87af-137ae32a610d" />

CasaNova supports local user management, authentication, and access controls.

And by default, CasaNova doesn't require a connection to a vendor-operated update service. Remote server endpoints are left unconfigured unless you choose to provide them.

Your hardware. Your applications. Your decisions.

---

## 🌱 Growing beyond its roots

CasaNova is a fork of CasaOS, and that's something we're happy to acknowledge.

CasaOS made self-hosting more approachable for countless people, and its ideas remain an important part of this project.

But CasaNova also has its own direction.

The goal is to build a home server platform that combines simplicity with greater control, giving enthusiasts room to experiment without making the experience unnecessarily complicated.

We're not trying to turn every home server into an enterprise datacenter.

We're trying to make the server sitting in your house a little more capable, a little more dependable, and a lot more enjoyable to use.

And we're still getting started.

---

## 📸 Take a look around

<img width="1919" height="999" alt="imagen" src="https://github.com/user-attachments/assets/d552a94a-22f6-4c2d-bcfe-3c45110fdf67" />

**The dashboard**

<img width="1611" height="1001" alt="imagen" src="https://github.com/user-attachments/assets/a601d224-d7df-4673-ade3-d1bd50360e48" />

**Applications and containers**

<img width="1490" height="887" alt="imagen" src="https://github.com/user-attachments/assets/8d256fc8-b2a7-4986-811f-f813c4ebb4ef" />

**Storage and files**

<img width="1916" height="998" alt="imagen" src="https://github.com/user-attachments/assets/598ea9e5-794a-4066-9bbe-c0a49a3bbfd0" />

**Settings and updates**

---

## 🚀 Getting started

### CasaNova 26.10 — Release 18.6.2

**A new chapter for your home server.** 🦁

CasaNova 26.10 brings together the improvements we've been building toward: a refreshed experience, a more resilient foundation, and a growing collection of tools designed to make self-hosting more comfortable.

This release is another step toward a platform that feels familiar, gives you more control, and grows alongside your needs.

| Release information | Value |
|---|---|
| Product version | **18.6.2** |
| Release series | **26.10** |
| Git tag | `v26.10` |
| Release title | CasaNova 18.6.2 — RELEASE 26.10 |
| Platform | Linux x86_64 |

For detailed changes and fixes, see [CHANGELOG.md](CHANGELOG.md) and the [GitHub Releases page](https://github.com/LeewayOpenSoftware/CasaNova/releases).

### 📦 Download CasaNova

Find the release package on [CasaNova Releases](https://github.com/LeewayOpenSoftware/CasaNova/releases).

**Release archive:** `casanova-18.6.2-RELEASE-26.10-20261018-x86_64.tar.gz`  
**Checksum file:** `SHA256SUMS`

After downloading both files to the same directory, you can verify the archive before installation:

```bash
sha256sum -c SHA256SUMS
```

### 🛠️ Installation

From the directory containing the downloaded archive:

```bash
mkdir -p /tmp/casanova-install

tar xzf casanova-18.6.2-RELEASE-26.10-20261018-x86_64.tar.gz \
  -C /tmp/casanova-install

sudo bash /tmp/casanova-install/install.sh
```

When installation finishes, open your server's address in a browser:

```text
http://<server-ip>/
```

Follow the welcome screen to set up your administrator account.

**Coming from CasaOS?**

CasaNova retains compatibility provisions for existing CasaOS installations, configuration paths, and application metadata. Back up your important application data and configurations before upgrading.

### 🔄 Keeping CasaNova up to date

You can install a compatible update package using the installer above or through the web interface:

- **Browser upload:** Settings → Update → Upload `.tar.gz`.
- **Local storage:** Discover update packages already stored on the server.
- **Mounted devices:** Find compatible packages on USB drives or other mounted storage.

The update system validates packages and uses the same update engine across these methods, reducing the need for manual SSH maintenance.

---

## 🧩 What's under the hood?

CasaNova combines a Go backend with a modern Vue 3 frontend.

Rather than putting everything into one large process, the backend is divided into cooperating services.

| Component | What it does |
|---|---|
| **CasaNova Core** | Dashboard, file management, and system features |
| **Gateway** | Routes requests and provides gateway-level protection |
| **App Management** | Manages Docker applications and Compose stacks |
| **User Service** | Authentication, accounts, and permissions |
| **Local Storage** | Disks, mounts, SMART, mergerfs, and rclone |
| **Message Bus** | Events and communication between components |
| **Job Management** | Scheduled and on-demand tasks |
| **Common** | Shared libraries, types, and SDK helpers |
| **UI v2** | Vue 3, Pinia, and PrimeVue interface |
| **CLI** | Command-line management tools |

The services communicate through a central gateway, with support for TLS and request rate limiting.

### Technologies

- **Go** for backend services.
- **Vue 3** for the user interface.
- **Pinia** for frontend state management.
- **PrimeVue** for UI components.
- **Docker and Compose** for application management.
- **systemd** for service management on Linux.

The goal is a platform that's approachable from the browser while remaining understandable and maintainable under the hood.

### Building from source

You'll need:

- Go 1.22 or newer.
- Node.js 20 or newer.
- A Linux build environment.

```bash
# Build the backend
for d in CasaNova CasaNova-Gateway CasaNova-AppManagement \
         CasaNova-UserService CasaNova-LocalStorage \
         CasaNova-MessageBus CasaNova-JobManagement CasaNova-CLI; do
  (
    cd "$d" &&
    go build -o "../build/full/bin/$(basename "$d" | tr A-Z a-z)" .
  )
done

# Build the frontend
(cd CasaNova-UI-v2 && npm ci && npm run build)

# Package the release
(
  cd build &&
  tar czf "casanova-$(date +%y%m%d%H%M)-x86_64.tar.gz" -C full .
)
```

The repository also includes a Makefile for consistent builds and version injection.

---

## ⚙️ Configuration

CasaNova stores its primary configuration under:

```text
/etc/casanova/
```

Compatibility files may also be maintained under `/etc/casaos/` during the transition from the original CasaOS architecture.

| File | Purpose |
|---|---|
| `casanova.conf` | Main service configuration |
| `gateway.ini` | Gateway ports and TLS settings |
| `app-management.conf` | Application catalogs |
| `user-service.conf` | Authentication configuration |
| `local-storage.conf` | Storage and mount settings |
| `message-bus.conf` | Event service settings |
| `job-management.conf` | Scheduled task settings |

You can add or replace application catalogs by editing `app-management.conf`.

Optional remote update endpoints can be configured if you operate your own update infrastructure.

---

## 🌱 What's next?

### Looking ahead to CasaNova 27.04

The 26.10 release is a milestone, not the finish line. **CasaNova 27.04** is the next development series: a place to explore new ideas, refine the experience, and keep improving the platform's foundations.

We want to make self-hosting more capable without losing the simplicity that made CasaOS appealing in the first place. We'll share concrete features and timelines as they're ready, rather than promise what hasn't shipped.

One release at a time. There's more to come. 🦁

---

## 🤝 Built on open-source foundations

CasaNova wouldn't exist without the work of the [IceWhale Technology](https://github.com/IceWhaleTech) team and the CasaOS community.

The original dashboard concepts, application ecosystem, installation approach, and much of the underlying foundation came from CasaOS.

We continue to preserve upstream attribution and compatibility wherever practical.

CasaNova is not intended to erase that history.

It's our way of building upon it.

### Contributing

CasaNova is an evolving open-source project, and thoughtful contributions are welcome.

If you've found a bug, have an idea, or want to improve something, start by opening an issue.

For larger changes, please discuss your proposal before submitting a pull request.

We loosely follow [Conventional Commits](https://www.conventionalcommits.org/) for commit messages.

---

## ❤️ A little more about CasaNova

CasaNova began as a personal home-server project.

Not because the world desperately needed another dashboard, but because there were things we wanted to do with our own servers that existing solutions didn't quite provide in the way we imagined.

So we started changing things.

One improvement led to another. Some changes were small. Others meant rebuilding entire parts of the system.

And somewhere along the way, it became CasaNova.

There's still work ahead, and we're not pretending otherwise.

But the idea remains simple:

**Self-hosting should feel like freedom, not another chore.**

That's the kind of software we want to build.

---

## Credits

Created by **Leeway Open Software**, in collaboration with Claude and GPT.

See [CREDITS.md](CREDITS.md) for additional acknowledgements.

## License

CasaNova is released under the **Apache License 2.0**, matching its CasaOS foundations.

See [LICENSE](LICENSE) for details.

---

**CasaNova 🦁 — Make yourself at home.**
