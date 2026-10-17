# ADR-0004: Component Startup Timing Determination and Ready Handshake

- Status: Denied
- Date: 2026-10-11
- Decision Owners: Nervus-OS Core Team
- Writers: opus 5.5 (gohj99)

Corresponding architecture document version: [NervusOS system architecture document](https://github.com/Nervus-OS/Nervus-Console/blob/master/architect/NervusOS-System-Architecture-Document.md)
Involved paragraphs: 3.4, 3.5, 6.1

## Problem

Document 3.5 specifies the lifecycle of the Service component:

> Its start corresponds to the `_start` symbol of the executable file

This statement cannot be implemented at the implementation level. The reasons are as follows.

### 1. `_start` is a linking concept, the kernel cannot observe

`_start` is not a contract provided by the kernel, but rather an **entry symbol** agreed upon by the **linker**. The `e_entry` field in the ELF header is the actual target that the kernel jumps to, and the relationship between `e_entry` and `_start` varies with the toolchain:

| Program form | `e_entry` points to |
| --- | --- |
| Dynamically linked C program | **Entry point of the dynamic linker ld.so**, then jumps to `_start` → `__libc_start_main` → `main` |
| Static linked C program | `_start` in `crt1.o` |
| Go program | `_rt0_amd64_linux` |
| Rust `#![no_main]` | Customized by the developer, symbol name is arbitrary |
| `ld -e <symbol>` | Any symbol specified during linking |

When the kernel returns from `execve`, **it only has the PID and address space, and does not know, and cannot know, from which function the target program starts executing**. Binding the lifecycle to `_start` is equivalent to requiring the implementer to wait for an event that the kernel cannot observe.

### 2. Timeout timing lacks a starting point

Document 3.4 specifies for the `auto` mode:

> If the process does not exit within the specified timeout and there are no other calls, the kernel automatically terminates its process

The starting point of the timeout is undefined. If it is calculated from ``_start`` as stated in 3.5, the kernel must wait for an unobservable symbol, making it impossible to start the timing.

### 3. Unable to distinguish between "alive" and "ready"

The smallest observable fact the kernel can detect is "execve succeeds, the process exists". However, the existence of a process does not mean it can handle requests—it may still be loading models, connecting hardware, or initializing buffers. The document currently has no means to distinguish between the two.

## Solution

### Solution 1: Use exec success as the starting point, without introducing a ready signal

Once the kernel `fork` + `execve` succeeds, the component is considered to have entered the "running" state, and the timeout starts from this moment.

- Advantages: Consistent for all languages and all binary forms; components do not need to cooperate; the implementation is the simplest
- Disadvantages: Cannot distinguish between "the process is alive but not ready" and "ready". The caller can only discover that the component has not started when the first request times out

### Solution 2: Use exec success as the starting point, and enforce the ready handshake

All Service components must notify the kernel through a specified FD after initialization is complete, otherwise the startup is considered a failure.

- Advantages: The ready timing is precise, and deadlocks can be detected
- Disadvantages: **Force all components to implement the handshake logic**, including C-written components that do not want to introduce additional dependencies; the mechanism of passing FDs increases the cognitive burden on component developers

### Solution 3: Use exec success as the starting point, and the ready handshake is optional

The kernel provides a ready FD when `fork`ing, and the component **may choose** to use it:

- Use the handshake → the kernel considers the handshake signal as the ready moment
- Do not use the handshake → the kernel considers the `execve` success as the ready moment (equivalent to Solution 1)

- Advantages: Balances both needs; does not force components to cooperate; zero cost for simple components
- Disadvantages: Two ready semantics exist in the same system, and the document must clearly explain them

## Decision

**Adopt Solution 3, optional ready handshake.**

### 4.1 Lifecycle Definition

Redefine the Service component lifecycle using **events observable by the kernel**:

| Stage | Determination Criteria |
| --- | --- |
| Start | Kernel `fork` + `execve` succeeds, and the process PID is obtained |
| Ready | Received ready signal; if the component does not use the handshake, it is equivalent to start |
| Running | After ready, during the process's lifetime |
| Exit | The process terminates on its own, or the kernel sends SIGTERM/SIGKILL |

**"Start" is no longer bound to any symbol, and the expression of the "_start" symbol is removed from document 3.5.**

### 4.2 Timeout Timing Start

Document 3.4's `auto` mode timeout, and the waiting time of `component_stop` in 6.1, both start counting **from the moment the component enters the 'Ready' state**.

If the component uses a ready handshake, the time between 'Start' and 'Ready' is constrained by the `component_ready` timeout (see 4.4); if no handshake is used, they are the same moment.

### 4.3 The Contract of Ready Handshake

**FD Passing Mechanism:**

The kernel creates an anonymous pipe when `fork`ing, and passes the write end to the child process via `SCM_RIGHTS`. The child process side does not use hard-coded fixed fd numbers, but instead **the kernel informs the fd number via an environment variable**, and the component reads this environment variable to obtain it.

**Semantics of the Three Signals:**

| Component Behavior | Kernel Judgment |
| --- | --- |
| Writing 1 byte to this FD | **Ready** — lifecycle enters 'Running' |
| Closing this FD without writing | **Start Failed** — kernel reclaims the process and records the error |
| Never writing and never closing | Waiting for `component_ready` timeout → **Start Failed** |

The third case is the core value of the handshake: it allows distinguishing between 'initialization deadlock' and 'normal initialization in progress'.

**Logs Do Not Use This FD:**

The component's stdout/stderr is redirected by the kernel to the log manager (see `deploy/journald-nervus.conf`). The ready FD only carries the 'Ready / Failed' bit of information, and is not mixed with logs to avoid parsing ambiguity.

**Survival Probe and Ready Probe Are Separated:**

The nervus-ipc protocol has defined Ping / Pong (`nervus_ipc.proto` field numbers 60/61), used to determine if the peer is alive. The ready handshake answers a different question — 'whether the process is able to handle requests'. They complement each other and are not mutually exclusive:

- Survival: Determined by Ping/Pong or process status, **always available**
- Ready: Determined by FD handshake, **component optional**

### 4.4 Configuration Items

Document 6.1 needs to add an item:

```yaml
system:
  timeout:
    component_ready: 30s # the longest a component may take to become ready; past it, the start counts as failed
```

Components that do not use ready handshaking are not subject to this item.

## List of Changes to the Document

| Paragraph | Change |
| --- | --- |
| 3.5 | Remove "the `_start` symbol corresponding to the executable"; redefine the lifecycle according to the table in §4.1 |
| 3.4 | Clarify that the `auto` mode timeout starts from "ready" |
| 6.1 | Add the `component_ready` configuration item; note the timing start point for `component_stop` |
| New paragraph | Supplement the ready handshaking contract in 3.1.2 or 3.5 (content from §4.3) |

## Others

- **Specific conventions for FD numbers** (environment variable names, pipe vs socketpair) are to be determined during implementation. This ADR only fixes two points: "not hardcoding fd numbers" and "three signal semantics".
- **App components are not subject to this ADR.** The runtime carrier of App is the display process, and its "ready" is defined by the first frame rendering between display and displayd, which falls under the scope of Chapter 5.
- **If it is later found that mandatory handshaking is indeed necessary** (for example, system services generally require precise ready signals), the solution 3 can be changed to solution 2. In this case, only the default value of the configuration in 6.1 needs to be adjusted, and the lifecycle definition (§4.1) does not need to be changed.
