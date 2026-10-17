# ADR-0003: Persistent Directory Layout and Access Control

- Status: Denied
- Date: 2026-10-11
- Decision Owners: Nervus-OS Core Team
- Writers: opus 5.5 (gohj99)

Corresponding architecture document version: [NervusOS System Architecture Document](https://github.com/Nervus-OS/Nervus-Console/blob/master/architect/NervusOS-System-Architecture-Document.md)
Involved sections: 3.1.2.1, 3.2.1, 3.2.2, 3.2.0, 4.5.1, 6

## Issues

Paths related to component persistent storage in the architecture document currently exist in three incompatible descriptions:

| Document Location | Current Description | Key |
| --- | --- | --- |
| 3.2.1 (near line 946) | Code directory `/var/lib/nervus/packages/<package_id>` | Package ID |
| 3.2.2 (line 946) | Data directory `/var/lib/nervus/package-data/<package_id>/` | Package ID |
| 3.1.2.1 (line 618) | Service data directory `/var/lib/nervus/service/<uid>` | Service UID |
| 3.1.2.1 (line 620) | System service shared directory `/var/lib/nervus/service/system` | Service group |
| 4.5.1 (line 1086) | `nervus.perm.datapath` → `/var/lib/nervus/data/` | Shared root |

This brings three specific issues:

### 1. Using UID as the path primary key is unstable in reinstallation scenarios

UID is a runtime identity (which identity the process runs as), while the path is a persistent identity (where the file is stored). Their lifecycles are different: Document 3.2.0 has specified that UID remains stable during upgrades, but **whether UID is reused after uninstallation and reinstallation is not specified**.

If reused, the new package may inherit the directory content and residual ACLs from the old package; if not reused, the `WorkDir` / `DataDir` paths drift with UID, and any location that has recorded the path (component registry, ACL, working directory of running processes) must be updated synchronously. Neither outcome is ideal.

### 2. The authorization scope of `nervus.perm.datapath` may exceed its intended boundaries

`nervus.perm.datapath` is a user group-controlled permission, GID 20002, which can access `/var/lib/nervus/data/`.

However, the component data directory is located at `/var/lib/nervus/package-data/<package_id>/` according to 3.2.2 —— **once this directory grants any group permissions (read/write/enter) to GID 20002, all packages that hold this permission can read data from other packages beyond their intended scope.** When users confirm "data path access permissions," the actual semantic meaning exceeds the interface description.

### 3. The trust boundary within a package is undefined

A package can have 1 App and any number of Services. The data directory is divided by package ID, meaning **the App and Service within the same package share the same data directory**. The document does not clarify whether this is an intentional trust model or an oversight.

## Solutions

### Solution 1: Code and data both use package_id as the key, shared root independent

```plain
/var/lib/nervus/
├── packages/<package_id>/        # Component code, root owns, component is read-only
├── package-data/<package_id>/    # Component data, owner is the UID of the package runtime
└── data/                         # Shared data root, GID 20002 controls
    └── downloads/
```

- Component identity and path are decoupled, path remains constant after upgrade/reinstallation
- `datapath` only covers `/var/lib/nervus/data/`, physically isolated from private data directories of each package, no risk of overflow
- Shared data directory among components of the same package, trust boundary is the package boundary

### Solution 2: code uses package_id as key, data uses UID as key

```plain
/var/lib/nervus/
├── packages/<package_id>/        # Component code
├── service-data/<uid>/           # Each component's independent data directory
└── data/                         # Shared data root
```

- Each component has an independent data directory, naturally isolated within the package
- Data paths drift with UID, reinstallation behavior requires additional specifications
- Components in the same package cannot share data, if sharing is needed, a separate directory must be opened, making the model more complex

### Solution 3: Maintain the Status Quo

Keep three sets of paths, and explain their purposes and relationships in the documentation.

## Decision

**Adopt Solution 1.**

### 3.1 Directory Definition

| Path | Owner | Permission Mode | Purpose |
| --- | --- | --- | --- |
| `/var/lib/nervus/packages/<package_id>/` | root:root | `0755`, and write permissions are removed for the component UID | Component code, component read-only |
| `/var/lib/nervus/package-data/<package_id>/` | The UID of the package runtime | `0700` | Component data, all components of the package can read and write |
| `/var/lib/nervus/data/` | root:root | `0770`, group is GID 20002 | Shared data root, controlled by `nervus.perm.datapath` |
| `/var/lib/nervus/data/downloads/` | root:root | `0770`, group is GID 20003 | Shared download directory, controlled by `nervus.perm.downloads` |

> The specific UID and permission mode are determined according to the principle of minimal permissions during implementation, this ADR only fixes the **selection of keys** and **isolation boundaries**.

### 3.2 Invariants

1. **Code and data are both keyed by `package_id`**, UID is only used for runtime identity, and not as part of any persistent path.
2. **UID is never reused after uninstalled**, it always increments. This is a supplementary regulation for 3.2.0 "Prohibit modifying UID during upgrade".
3. **No permission can access `/var/lib/nervus/package-data/<package_id>/` that does not belong to it.** This directory is not included in any permission group's ACL coverage, its access is entirely determined by the owner and permission mode.
4. **A component accessing its own data directory requires no permission.** `nervus.perm.datapath` only covers the shared root `/var/lib/nervus/data/`.

### 3.3 Trust Model

**The boundary of a package is the trust boundary, and components within the same package trust each other.**

Reason: A package is signed by the same signer (see 3.0.2), and the App and Service of the same package in manifest.json are published by the same developer, therefore sharing the data directory among components within the package does not constitute privilege escalation. This will lead to a result that must be accepted:

> **Any component that can read the package's data directory is equivalent to being able to read all data of all components in the package.**

Therefore, the requirements are:

- App components (JavaScript, running in display sandbox) that share the data directory with the same package Service must strictly limit the file access range within the sandbox to this directory
- Document 3.1.1.7 "Users can freely install user applications" — the installed package by users also applies to this model, no exceptions allowed

### 3.4 Component Registry Fields

In the `Component` structure:

- `WorkDir` = `/var/lib/nervus/packages/<package_id>/`
- `DataDir` = `/var/lib/nervus/package-data/<package_id>/`

The values of the two fields are determined only by `package_id`, not by `uid`. The `WorkDir` and `DataDir` of components in the same package are the same.

### 3.5 Handling of Legacy Paths

The original `/var/lib/nervus/service/<uid>` **is no longer considered a standard path**. Services are subclasses of components, and their data directories are consistent with other components, using `package-data/<package_id>/`.

The original system service shared directory `/var/lib/nervus/service/system` **is retained but needs to be relocated**: it is the directory for "all system components shared," which is a different level of concept from "shared within a single package." It is recommended to change it to `/var/lib/nervus/data/system/`, which is included in the shared root, controlled by GID 20002 or an independent system service group. This item will be documented together after confirmation.

## List of Document Modifications

After this ADR is accepted, the architecture document needs to be modified as follows:

| Paragraph | Modification |
| --- | --- |
| 3.1.2.1 | Remove the description of `/var/lib/nervus/service/<uid>`; the system service shared directory is changed to a subdirectory under the shared root |
| 3.2.1 | Clarify the read-only permission mode for `packages/<package_id>/` for components |
| 3.2.2 | Clarify the owner as the UID of the package runtime, permission mode `0700`, and all components of the package can read and write |
| 3.2.0 | Add "UID unloading does not reuse"; the value rules for `WorkDir` / `DataDir` |
| 4.5.1 | Change the description of `datapath` to "shared data root access permissions," and note that it **does not override** the private data directories of each package |
| 6 | Add a subsection on directory layout (the table from §3.1) |

## Others

- **This ADR does not involve the design of the permission registry itself** (how built-in permissions are initialized, the relationship between `permission_additions` and package-level permissions), which belongs to the scope of 3.1.2.3.2 and 4.3, and requires a separate ADR.
- **`IsAvailable string` should be `bool`, `ComponetPermission` spelling** and other typos have been directly corrected in the document and are not within the scope of this ADR.
- If the column "Accessible Files / Directories" in the permission table contains placeholders (such as `<package_id>`), the semantics should be clearly defined in the document as either "the package to which the caller belongs" or "any package" — according to the decision of this ADR, only the former is meaningful.
