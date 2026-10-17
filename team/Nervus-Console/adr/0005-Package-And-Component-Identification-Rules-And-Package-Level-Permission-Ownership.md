# ADR-0005: Package and Component Identity Rules and Package-Level Permission Ownership

- Status: Denied
- Date: 2026-10-11
- Decision Owners: Nervus-OS Core Team
- Writers: opus 5.5 (gohj99)

Corresponding architecture document version: [NervusOS System Architecture Document](https://github.com/Nervus-OS/Nervus-Console/blob/master/architect/NervusOS-System-Architecture-Document.md)
Involved paragraphs: 3.0.1.1, 3.0.1.5, 3.1.2.3.2, 3.2.0, 3.3, 4.2, 4.3

## Problem

There are two rules in the document about the identity of "package" and "component", which are currently not rigorous enough and are coupled with each other.

### 1. The expression "with package as a prefix" has a privilege escalation vulnerability

Paragraphs 3.0.1 (line 218) and 3.0.1.5 (line 284) specify:

> When a component ID is prefixed with package, it is considered as a component in the same package, otherwise it is considered as using a shared package.

**If the "prefix" is understood literally as a string, the package `com.example.app` will incorrectly match the component `com.example.application`.** This component belongs to no package, yet it will be judged as a component in the package `com.example.app`, thus gaining the permissions and resource access scope declared by that package. This is a real privilege escalation path.

The architecture acceptance is also not strict: `com.example.app` is a three-segment dotted string. If allowing declaration of longer package IDs (such as `com.example.app.evil`), the same prefix matching problem will reappear in a more subtle form.

### 2. The location of package-level permission synchronization is undefined

Paragraph 4.3 (line 1064) specifies:

> When a permission is successfully declared for a component in a package, it should take effect synchronously for all components in that package.

But it does not specify **where** the "confirmed" state is **stored**. The current implementation (`internal/registry/component_store.go:170` of `ConfirmPermission`) records it on **a single component's single permission** (`ComponentPermission.IsConfirmed`).

This leads to potential inconsistency between the states of two components:

- B is a service in the package and declares `permission_additions`
- A is in the same package as B, and when A calls B, `fillCallerPermissions` will **skip A's own permission with `IsConfirmed == false`**
- Result: the `caller_permissions` obtained from the same-package call is empty

This contradicts the document's statement in 3.3 (line 952) that "for calls initiated by components in the same package, the kernel should directly pass through" and the package-level synchronization rule in 4.3.

**The root of the problem is: is "permission confirmed" a property of the component or the package?** The document says it is package-level, but the implementation is component-level.

## Solution

### Solution 1: Fix the package ID to three segments, record permission grants on the package

- The package ID format is tightened to **exactly three segments** (`com.example.app`), and the component ID is the package ID followed by any number of segments, **matched by splitting with `.`**
- The "confirmed" state is recorded on the **package**, inherited by components upon registration, and not stored separately

### Solution 2: Package ID segments are not limited, permission grants are recorded on the package

- The package ID remains in reverse domain format but is not limited in the number of segments, and the matching is also tightened to prefix segments
- Permission ownership is the same as Solution 1

### Solution 3: Package ID segments are not limited, permission remains on the component, managed by the package manager for synchronization

- The matching rule is tightened to prefix segments
- Every time a permission is confirmed, the package manager traverses all components in the package and writes `IsConfirmed` for each one

### Solution 4: Maintain the current state

Do not tighten the matching rules, and do not change the permission ownership.

## Decision

**Adopt Solution 1.**

### 4.1 Package ID Format

**The package ID must consist of exactly three segments**, separated by `.`

```plain
com.example.app     ✓
org.nervus.demo     ✓
com.example         ✗ Not enough segments
com.example.app.x   ✗ Too many segments
```

The purpose of the three-part restriction is first to make the boundary of prefix matching clear, and second to make the relationship "package ID is a strict prefix of component ID" unforgeable—attackers cannot fall into the prefix range of a legitimate package by declaring `com.example.app.evil`.

**The composition of a package is bound to the signer's identity in 3.0.2**, the uniqueness of the package ID is guaranteed by signature verification and version conflict check in 3.0.1.1.

### 4.2 Rules for Determining Whether Components Are in the Same Package

> A component ID **equals** the package ID, or starts with **the package ID followed by `.`**, it is considered as a component in the same package. Otherwise, it is considered as using a shared package.

Examples and counterexamples:

| Package ID | Component ID | Determination |
| --- | --- | --- |
| `com.example.app` | `com.example.app` | Same package |
| `com.example.app` | `com.example.app.main` | Same package |
| `com.example.app` | `com.example.app.service.foo` | Same package |
| `com.example.app` | `com.example.application` | **Different package** (does not match) |
| `com.example.app` | `org.other.thing` | Different package |

**Implementation Constraints:**

1. **The determination is based on the `Component.PackageID` field, not on string prefix comparison at runtime.** `package_id` is filled by the package manager when the package is loaded based on the manifest, and it can be directly compared at runtime (the field `internal/registry/component.go:31` already exists).
2. The string prefix rule **is only used to determine ownership during loading** (the package manager checks whether the component ID in the manifest belongs to the current package), and **during registration validation**. It is a validation method, not a determination basis.
3. Comparisons must be done by splitting on `.` and must not use `strings.HasPrefix` for direct comparison.

### 4.3 Permission Ownership is at the Package Level

**The "permission has been confirmed" record is stored at the package level, not at the component level.**

Specific meaning:

- In the permission registry, "whether a package has obtained permission P" is a **package-level state**
- Components within the package are **inheritors** of this state and do not hold independent confirmation bits
- The `Component.Permissions` field only indicates "which permissions this component declares", **no longer indicating whether it has been confirmed**

Thus, the "synchronous effect" in 4.3 is no longer an action that needs to be executed, but rather a property of the model itself—**components in the same package cannot have inconsistent confirmation states**.

**Impact on `fillCallerPermissions`:** The criterion changes from "whether the calling component's permission is `IsConfirmed`" to "whether the **package to which the caller belongs** has obtained the permission." This makes same-package calls naturally valid, consistent with 3.3.

### 4.4 Relationship with `permission_additions`

It is necessary to distinguish between two different mechanisms in the document to avoid confusion for implementers:

| Mechanism | Problem Controlled | Determination Basis |
| --- | --- | --- |
| 3.3's "same-package direct access" | Whether a component **can be started** | Whether it is in the same package |
| `permission_additions` (3.1.2.3.2) | Which **custom permissions** the caller **has** | The intersection of permissions declared and confirmed by the caller's package and the `permission_additions` declared by the target service |

The two are at different levels: the former is about execution permission, the latter is about access permission. **Same-package can be exempt from `nervus.system.perm.execute` check, but it does not mean automatically obtaining all the custom permissions declared by the target service**—that still depends on the package-level permission grant in 4.3.

> To be confirmed: If the product intent is "same-package calls automatically obtain all `permission_additions` declared by the service in the same package," then it needs to be explicitly stated here. In this case, the package-level grant in 4.3 is a sufficient condition, and no additional application is needed. **Please clarify this item.**

### 4.5 Sources of Built-in Permissions

Document 4.1 specifies that "the source of all built-in permissions is the kernel virtual service `nervud.system.server`." This rule does not conflict with package-level granting, but note that:

- **User Group Controlled Permissions** (4.5.1, GID 20000–20005) are initialized by the kernel at boot time to create groups and ACLs, and are unrelated to package-level granting
- **Permissions requiring user confirmation** trigger the confirmation process via `nervus.service.permission`, and the confirmation result is recorded on the package as per 4.3

## List of Document Modifications

| Paragraph | Modification |
| --- | --- |
| 3.0.1.1 | Clarify that package ID must be three-segment |
| 3.0.1 (line 218) | Change ``with package as a prefix`` to ``equal to the package ID, or start with the package ID followed by `.``` |
| 3.0.1.5 (line 284) | Same as above, and add the positive and negative examples table in §4.2 |
| 3.2.0 | Change the semantic of the `Component.Permissions` field to ``the permissions declared by the component``, and no longer carry confirmation status |
| 4.2 | Add ``confirmed`` as a package-level status, and explain its storage location |
| 4.3 | Rewrite ``take effect synchronously`` as a model property, rather than a synchronous action |
| 3.3 / 3.1.2.3.2 | Add two mechanism comparison tables in §4.4, and clarify that they are not mutually implied |

## Other

- **This ADR is independent from ADR-0003**: The storage location of package-level permissions does not depend on directory layout, and vice versa. However, both involve the `Component` struct, and adjustments should be made simultaneously during implementation.
- **The design of the permission registry itself** (the initialization timing of built-in permissions, the structure of `/run/nervus/permissions.json`, and the method signature of virtual service 7.1.3) **is not within the scope of this ADR**, and must be defined separately in Chapter 7.
- **Current Implementation Status**: `internal/registry/component_store.go:170`'s `ConfirmPermission` currently only modifies a single component, `internal/socket/connection.go:225`'s `fillSubscribePermissions` is an empty shell, and `internal/virtual/response.go:74`'s `HasPermission` is not called. After this ADR is accepted, these three locations need to be rewritten according to the new model.
