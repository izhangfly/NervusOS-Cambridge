# Agent System Service Related

- Status: Create
- Date: 2026-08-16
- Writers: gohj99

Here temporarily use the document I wrote previously with AI, I will update it later when I have time

# AI Integration Direction

> This document only records the conclusions and open questions about "how to integrate AI into the system".
> Content unrelated to this (communication, packaging, UI, drivers, etc.) is not within the scope of this document.
> Status: Direction draft, not finalized, pending internal discussion.

---

## 0. One-sentence summary

**System deep integration of agent; software (mod) does not describe behavior, only expands the space of possibilities; behavior solidifies into solid state during use.**

- **mod model** answers "What is software?"
- **Phase change model** answers "How AI and software coexist"
- **Algebra / predicate / reference** are three forms of mod adding things to the system /no_think

---

## 1. Problem Definition

### 1.1 Two Paths We Reject

**A. System-Built Fixed Action Set (Skills)**

Enumerate actions like "making the bed" and "pouring water" into the system in advance.
Rejection Reason: The possibility space is locked by a table at the factory, making extensibility zero. This is the "phone that can only install official software."

**B. Prompt Directly Controls the Robot**

Software sends a piece of natural language, and the agent directly drives the executor.
There are three rejection reasons, any of which is fatal:

1. **Safety Has No Upper Bound**: There is nothing between AI errors and the robot moving. The error rate is always not zero, so the consequences must have a bound.
2. **AI in the Control Loop**: Model latency (100ms–1s) directly becomes control latency, making real-time performance unsolvable. This is a physical constraint, and no better model can fix it.
3. **Text Cannot Hold Permissions**: A sentence has no identity, cannot be revoked, and cannot be audited. Whoever can put text into the system has permissions—on the robot, this is a physical consequence.

### 1.2 The Real Goal

> Let AI freely act during runtime, while ensuring that the "consequences of AI making mistakes" have a definite upper bound.

Key Insight: **It's not about making AI more accurate, but making it unable to express the desire to make mistakes.**

Human arms are safe, not because the brain is reliable (the brain is not reliable at all: people fall, spill cups).
It is because: **The language sent from the brain to the arms has no words that can express dangerous actions.**

- Spinal reflex ~30ms, not passing through the brain, still works if the connection is cut
- Golgi tendon organs: automatically inhibit when tension exceeds limits, **the brain cannot command muscles to exceed the tendon's capacity**
- The brain sends not joint angles, but muscle synergies (synergy)

This is the mechanism to be replicated.

---

## 2. Core Model One: mod

### 2.1 Definition

> **Software does not describe behavior, software expands the space of possibilities.**

Refer to Minecraft's mod: mod does not specify how players play, it adds a new ore, new recipe, new mechanism, **players themselves discover what they can do with it**.

This form structurally cannot collapse back to skills — because it never says "what to do".

### 2.2 Five Types of Extension Points of mod

| mod Adds | Extension Base | Hotel Scenario Example |
|---|---|---|
| **Entity Type** (Noun) | Referent Layer | Room, Linen Room, Leftover Item, Room Status |
| **Predicate** (Judgment) | Predicate Language | Room.MeetsStoreStandard, Room.Cleaned |
| **Ability** (Verb) | Algebraic Primitive Combination | Swipe Room Card, Call Front Desk, Connect to PMS |
| **Constraint** (Not Allowed) | Safety Envelope | Not Enter Occupied Room, Not Touch Leftover Item |
| **Goal** (Should) | World State Declaration | Room That Has Checked Out Should Meet Standard |

### 2.3 Form Sketch

```yaml
mod: com.hotel.suite

extends:
  entities:                          # → Referent layer
    Guest room:   { Room number, Floor, Room status }
    Linen room: { Location, Stock }
    Leftover items: { Discovery location, Discovery time }

  predicates:                        # → predicate language
    Room.meetsStandard:              # photos define the room
      Definition method: examples/standard_room_*.snapshot   # photos define the room
    Room.isCleaned: floor.clear ∧ trash.empty ∧ bed.made

  capabilities:                      # → Must be composed of existing primitives
    Brush room card   ≜ approach(card reader) ; contact(card, card reader) ; wait stable
    Take spare blanket ≜ transport(bathroom.bedding → current room)

  constraints:                       # → Only tighten, never loosen
    - Do not enter room status ∈ {occupied, do not disturb}
    - Do not touch left-behind items

  goals:                             # → World state declaration, not task
    - ∀ room where room.state == checked out: room.meets store standards

integrations:
  pms: { webhook: /checkout, credential: ${PMS_KEY} }
```

**The entire mod does not mention a single word about "how to make a bed".**
It only says "the room upon check-out should meet the hotel's standards", defined by several photos.
Making the bed emerges from the agent + algebra + memory. This is proof that it is not skills.

### 2.4 Three Must-Adhere Rules

**① The mod cannot introduce new primitive operations; it can only combine existing ones.**
Third-party providing a new primitive operation called "pouring water" directly to the executor → all security arguments are invalid.
New capabilities must be combinations of already verified primitives.
Exception is only OEM vendors (see §6, Layer 3), signed and authorized separately.

**② The mod's predicates can only be used for goals and acceptance, never for security guards.**
Third-party predicate judging wrong → task does not converge or converges incorrectly → annoying but not dangerous.
Security guard using third-party predicate judging wrong → accident.
**Guards only use predicates provided by the kernel.**

**③ Constraints take intersection, capabilities take union, goals must arbitrate.**
- Constraints **intersection** —— only stricter, always safe
- Capabilities **union** —— only more, safe
- Goals **may conflict** ← the only dangerous place, see §7.1

### 2.5 One Declaration, Three Entrances

The capabilities and goals declared by the mod automatically appear in three places:

```
        com.hotel.suite Declare "Take spare bedding"
                    │
        ┌───────────┼───────────┐
        ▼           ▼           ▼
    Human's UI AI's Algebra Voice
    (Click button) (Composable primitives) ("Get a blanket")
```

Developer writes once, three entry points are available. Analogous to Android's Intent.

**Meaning: AI and humans are symmetric.** Two equal operators, using the same set of capabilities, subject to the same set of constraints.
Not "AI controls the robot, human supervises AI".

---

## 3. Core Model Two: Phase Change

### 3.1 Problem

> AI is alive, software is dead, how can they be combined?

**"Combining" is the wrong direction.** Alive and dead are two ends of the same axis: **when behavior is determined.**

| | Behavior Determined At | Obtain | Lose |
|---|---|---|---|
| Software | At writing time | Predictable, testable, accountable, repeatable | Respond to new situations |
| AI | At runtime | Respond to new situations | The above four things |

Each advantage is exactly the other's disadvantage, corresponding one by one.
Therefore, "combining" is likely to result in the drawbacks of both: neither predictable nor flexible. **The middle ground is the worst ground.**

### 3.2 Conclusion

> **Software is not the opposite of AI. Software is the state of AI after its output stops changing.**
> Two phases of the same thing, like water and ice.

```
   First time doing   → fully liquid. AI generated, slow (second level), may fail, need confirmation
   Repeat success   → start solidifying. parameters narrow, branches cut off, confidence rises
   Stable after   → solid. deterministic, millisecond level, testable, signable, distributable
   Environment changed   → melt. success rate drops → automatically revert to liquid, regenerate
```

Four mechanisms can all be implemented:

| Mechanism | Implementation |
|---|---|
| **Solidification** | Record successful expressions + parameter distributions, fit into parameterized strategies |
| **Melting** | Success rate drops below threshold → mark as failed → next time take AI path |
| **Temperature** | Each "task × environment" pair has a solidification degree. New environments have high temperature, familiar environments have low temperature |
| **Solidification Threshold** | Not only look at success rate, but also look at **diversity** —— succeed under different initial conditions counts as learning, otherwise it may just be luck |

### 3.3 Two Mature Precedents

**For the operating system direction: This is JIT.**

| JIT | Robot |
|---|---|
| Interpretive execution (slow, general) | AI generates sentence by sentence |
| Hotspot detection | Repeatedly occurring situations |
| Compilation optimization | Solidify into parameterized strategies |
| **Deoptimization** | **Environment changed, revert to AI path** |
| Inline cache | Referential cache |
| Type feedback | World state feedback |

JIT is a mature industrial-level example of "live compiler + dead code" coexisting, running for over twenty years, even deoptimization is mature.

**For the neuroscience direction: This is procedural memory solidification.**

Learning to drive: initially rely on the prefrontal cortex (slow, tiring, error-prone) → after mastery transfer to the dorsal striatum and cerebellum (fast, efficient, automatic) → **if the road condition becomes abnormal, immediately revert to conscious control**. Deoptimization is also available in humans.

Two fields independently evolved the same structure, which is a strong signal that this direction is reliable.

### 3.4 Which Problems Does the Phase Change Model Address

| Previous Problem | In the Phase Change Model |
|---|---|
| Too dead, like skills | Solid state is just a temporary phase, and it grows rather than being hard-coded |
| What to do if failure rate is too high | It is inevitable in the liquid phase, and it decreases with solidification |
| Does the developer need to handle various failures | No. Failures in the liquid phase are handled by the system, and solid phase has few failures by nature |
| AI delay is too big | Solid phase path does not call AI, millisecond-level |
| How to clarify semantics | The solidification process itself is a process of disambiguation, once asked, no need to ask again |
| Scalability | Infinite — new behaviors are generated in use, no need for anyone to predefine |

---

## 4. mod × Phase Change = Flywheel

Two models are orthogonal, combined into a closed loop:

```
     mod Expand Possibility Space
            ↓
     agent exploring inside (liquid: slow, asks questions, occasionally fails)
            ↓
     Valid action solidification (solid state: fast, silent)
            ↓
     Freeze results packaging → become new mod distribution
            ↓
     Others have installed, starting point is higher ──┐
            ↑             │
            └─────────────┘
```

**The App Store sells two things:**

1. **Capability Expansion Packs** —— Developers' mods (new nouns, new predicates, new capabilities, new goals)
2. **Solidified Experience Packs** —— Machine-learned (a certain model has mastered behavior in a certain environment)

The second is something others don't have, **because it's not written, it's grown.**

**Software is liquid at factory release, and solidifies in the user's home.** The first week is slow, requires asking, and occasionally fails; after a month it's fast, quiet, and almost error-free. Each machine solidifies into a different shape — because each home is different.

---

## 5. Five Common Foundations

**These five things are unrelated to "which agent path to choose in the end". Any plan must use them, so we can start working on them first.**

| Foundation | Purpose |
|---|---|
| **Reference Layer** | Why does "blanket.corner[0]" refer to that corner in the real world |
| **Predicate Language** | How to describe the state of the world (guards / verification / goals share the same set) |
| **Algebra** (primitives + operators) | How behaviors are composed to generate actions |
| **Memory** | Space / instance / program / context |
| **Shield / Envelope** | Real-time safety guarantee |

### 5.1 Reference Layer

**It is not "integrating everything in the world into a single table"** (that's Cyc's way of dying). It is: **scan, recognize, and save.**

- **Open-set recognition, not classification.** Things we haven't seen don't need labels; what is saved is "instance id + visual embedding + origin". Names can be added later or given by users — "this is my cup" is a one-time binding, just like how children learn words.
- **Instances take priority over categories.** It is not a member of `class BLANKET`, but rather "**this particular** blanket". This is how people recognize things.
- **Reference is a confidence-weighted ranking, not a lookup.** If multiple matches are found → resolve ambiguity with context, or **ask**.

**Reference errors are a type of error that the safety envelope cannot catch**: AI refers "blanket corner" to "cat tail", with no force exceeded, no area violated, no collision with people — but it goes to grab the cat.
**Countermeasures: confidence + explainability + human-in-the-loop.** Low confidence + high risk → stop and confirm. This is exactly how humans do it: you also look twice in poor lighting before reaching out.

**Key design: mod can use memory, but cannot see memory.**

```
mod:  "Bedroom bed"
System:  → Parse → Return opaque handle ref#8a3f
mod: Take ref#8a3f to assign a task
mod: Never know where the bed is, how big the bedroom is, how many rooms there are in the house
```

Stronger than Android's location permissions — Android at least gives the app the result of "you are at Starbucks," but here it doesn't even give the result; it only gives a reference.

> **The referential layer is simultaneously the capability layer and the privacy boundary.** The layer that must exist in order for the language to be able to speak, and conveniently solves the privacy issue as well.

### 5.2 Algebra (Primitives + Operators)

Bet: **Do not enumerate skills, define about 10 physical primitives and a closed set of combinatory algebra, everything is generated through combination.**
26 letters can form infinitely many words. **The vocabulary is closed and extremely small, yet the expressive space is infinite.**

```
Original terms (motion): approach · contact · constraint · release · transport · apply force · follow · wait for stabilization
Original terms (perception): observation
Operators: sequence · parallel · guard · until · selection · repeat
```

**Push Demo (Making a Bed)**

```
Grab(Hand, Point) ≜ Approach(Hand, Point) ; Contact(Hand, Point) ; Constraint(Hand, Point, 6DOF)

Smooth( hand, face ) ≜ Parallel( Contact( hand, face ), Force( hand, normal, 5N ), Follow( hand, face ) )
               until Observe( face.flatness > θ )

Fold(EdgeA, EdgeB) ≜
    Parallel( Grab(LeftHand, EdgeA.Point1), Grab(RightHand, EdgeA.Point2) )
  ; Constraint(LeftHand, RightHand, Distance = |EdgeA|)        ← Mutual constraint between hands, to prevent bunching
  ; Transport(LeftHand and RightHand → above EdgeB) Guard Force < 20N
  ; Release(LeftHand and RightHand)
  ; WaitStable                                  ← Let gravity finish the rest
  ; Flatten(RightHand, Cover)

Making a bed (bed) ≜
    Observe (bed) until identify (blanket, edge ≥ 2)
  ; Flatten (right hand, blanket surface)                      ← First flatten, otherwise it won't fold neatly
  ; Fold (blanket.long edge, blanket.opposite edge)
  ; Fold (blanket.short edge, blanket.opposite edge)
  ; Transport (blanket → bed.tail)
  Guard throughout: force < 25N ∧ in (bed area ⊕ 0.2m) ∧ avoid (person, 0.3m)
```

"Making a bed" is not a vocabulary entry, but an expression. Changing it to "folding clothes" is just changing parameters and the number of folds, and nothing needs to be added to any table.

**Six structural issues discovered during the simulation**

1. **"Grasping" is not a primitive** —— The essence of grasping is "constraining the degrees of freedom of an object relative to the hand," which is already covered by "constraint." Algebra is more efficient than expected.
2. **Missing primitive "wait for stability / let physics happen"** —— After placing the quilt, gravity completes the rest. A large part of the operation is "place properly, then let physics evolve on its own." Without it, deformable objects cannot be expressed. This is not a small patch: algebra needs the semantic meaning of "something not under my control, but I have to wait for."
3. **"Observe" has a different sort from other primitives** —— Motion primitives change the world, perception primitives change cognition. Algebra may need two sorts, like a type system that distinguishes values and effects.
4. **"Until" and "guard" are two sides of the same thing** —— "Until P" is a termination condition, "guard P" is an invariant throughout, both are predicates about the state of the world. Therefore, algebra must have a predicate language underneath, and it is exactly the acceptance predicate, and also exactly what is used to declare the state of the world. **The three are the same thing.**
5. **Where do parameters come from, algebra doesn't care** —— "bed.length," "bed.tail," and "θ" are handled by the referent layer. This again proves that the referent layer is independent and essential.
6. **"Parallel" hides resource arbitration** —— What if two hands fight for the same workspace? Algebra needs to know which primitives' scopes will conflict.

**The most tempting and difficult point: no one knows what the correct set of primitives is.** This is a truly open research question.
Crossing clues: Bernstein's **muscle synergy** theory says the human brain uses a small set of base vectors to control hundreds of muscles —— **this might be the set of primitives biology provides.**

### 5.3 Memory

Four layers, the reason for layering is **different lifecycle, different writers, and different permissions**, mixing them together will inevitably cause problems.

| Layer | Content | Who Can Write | Lifecycle |
|---|---|---|---|
| **Spatial Layer** | House type geometry, room semantics, passage zones / restricted zones | Only system perception service | Long-term, slowly evolving |
| **Instance Layer** | Object instance ID + visual embedding + last position + origin | Only system perception service | Medium-term, will decay |
| **Program Layer** | Solidified behavior + success rate statistics | Execution feedback loop | Long-term, relying on repeated reinforcement |
| **Scenario Layer** (cross-cutting) | When and why something happened, how decisions were made | Kernel | Append-only, never modified |

**The Program Layer is the solid-state storage of the phase-change model.**

**Forgetting Mechanism**

- **Instance Layer naturally decays**: Confidence in things not seen for a long time decreases, and if it drops below the threshold, it needs to be reconfirmed. A cup not seen for half a year may have already been thrown away.
- **User actively forgets**: "Forget what happened in the study room" must be a first-class operation, a privacy necessity.
- **Program Layer de-solidifies**: If the success rate drops, it automatically reverts to the AI's slow path to relearn. If the bed or the quilt material changes, the old solidified action becomes invalid —— the system should detect this and degrade on its own, rather than continue to make mistakes. This is also how humans behave: once an action becomes awkward, we start doing it consciously again.

**Pollution and Audit**: Memory being corrupted is more dangerous than code being corrupted, and it's extremely hard to detect —— it won't crash, but it will make the machine gradually become wrong. The write path must be monopolized by system services; mod can contribute annotations but must carry the source, and annotations should be stored separately from perception facts, always distinguishable.

**Strategic Judgment: Memory is the only irreplaceable asset of this machine.** Models can be replaced, hardware can be replaced, but "it knows what your home is like" cannot be replaced. At the same time, this means that the house map is extremely sensitive data —— which software can read your home floor plan is more serious than which software can control a robotic arm.

---

## 6. Three Layers of Control Coexist

**Each of these three layers has its own ABI, and there is no requirement for uniformity. Layers 2 and 3 do not require AI and can be implemented independently first.**

```
Application
 ├─ Layer 1: AI Path     ← mod + phase change + algebra. Main content of this document
 ├─ Layer 2: Coarse Control   ← coordinate + boundary, synchronous call, determinism, millisecond level
 └─ Layer 3: Fine Control    ← OEM native service, high privilege + vendor signature
```

Three points to note:

1. **Layer 2 is not a downgrade solution; it is the foundation of Layer 1.** The AI path eventually has to come down to coordinates and boundaries. It is the only channel that is fully controlled by the safety gate.
2. **Not all software needs AI.** The inspection app only needs to ``go to point A, take a photo, go to point B, and upload''. Giving it AI is a waste and a risk. **This type of software may make up the majority, and also be the first batch that can run smoothly.**
3. **Using synchronous, typed, deterministic SDKs for Layers 2 and 3 is the correct approach**, and is not affected by the debates in Layer 1.

---

## 7. Latency and Execution Model

**Core: AI is not in the control loop.**

| Layer | Latency | Who is running |
|---|---|---|
| Shield (Envelope Enforcement) | **< 1ms**, 1kHz control loop | Deterministic code, zero allocation |
| Solver (QP / IK / LNN) | 1–10ms | Deterministic + small model |
| Solid Path Hit | 10ms level | Lookup + solver |
| **Liquid Path (Edge-side LLM generation)** | **100ms–1s** | Small model |
| Full Context Convergence | 30s–5min | All of the above |

```
  t=0 Submit ──────────→ Immediately return handle (~ms)
  t=0.3s AI Generate Algebraic Expression ──→ AI Exit
  t=0.3s+ Solver + Shield Real-time Execution ←── Here No AI
  t=45s Predicate Evaluation ──→ callback
           ↑
           Only call AI again when execution fails and re-planning is needed
```

**This is the second reason for "algebra" rather than "prompt-controlled": it's not just about safety, but also real-time performance.**
If AI continues to generate movement, its latency **is** the control latency. Algebra allows AI to leave the loop.

### Failure recovery belongs to the agent, not the mod

Failures in the physical world are open sets; developers can't enumerate them all. Moreover, different mods encounter the same failures (e.g., "no blanket" is the same for hotels and home use). Letting each developer write their own version is privatizing a common problem.

**Mods declare not failure branches, but three things:**

| Declaration | Meaning |
|---|---|
| **Recovery Space** | What the agent is allowed to do when self-rescuing (can it move obstacles? can it go to another room?) |
| **Escalation Target** | Who to ask when it can't be solved (which user, which role, which system) |
| **Abandonment Condition** | When to stop (timeout, number of attempts, battery, cost limit) |

**Mods never enumerate failure modes; they define the boundaries of self-rescue.**

**"Human takeover" is not remote control, but an authorization of one question and one answer:**
> "Room 302 didn't find a blanket, go to the linen room to get one?" → Yes / No

**And this UI is provided uniformly by the system, not written by mod developers.** Analogous to Android permission popups: mods only declare "who to ask, what scope" and write no UI code at all.

**Developers only handle one type of failure: final failure. All intermediate states are hidden.**

### How to make semantics clear: acceptance predicate is the only true source

> **Semantics is not that natural language. Semantics is the acceptance predicate. Natural language is just a prompt for AI, not part of the specification.**

If AI completely ignores the description, but satisfies the acceptance predicate and stays within the envelope the whole time — **that's success**.
Thus, the concept of "agent going off track" is eliminated: going off track = not satisfying the acceptance predicate = failure, which the machine can judge. **There is no "it thought it was doing the right thing."**

Four narrowing methods (strong → weak):

1. **Acceptance predicate is the only true source**
2. **Reference must be pre-bound; AI is not allowed to introduce new nouns** —— noun list is closed. This eliminates the largest space of error, as most "going off track" essentially refers to the wrong thing
3. **Parameters are typed slots, not free text**
4. **Examples are specifications** —— a photo of a neatly folded blanket is more precise than a hundred-word description

Bottom line: **If confidence is below the threshold, reject the action**, call back `AMBIGUOUS` and attach candidates. Don't guess.

---

## 8. Safety Model

### 8.1 Liquids Can Only Exist Where The Cost Is Acceptable

**The fundamental difference between robots and ordinary software is not real-time: software can be restarted, but the world cannot.**
Traditional software practices (idempotence, retries, rollbacks, snapshots) all fail in the physical world. A broken bowl is broken.

Label each primitive with the **upper bound of the worst consequence**:

| Action | Worst Consequence | Is AI Allowed to Act Freely? |
|---|---|---|
| Approach / Observe / Smooth | None | ✅ Full Freedom |
| Pick up, put down, carry (light objects) | Reversible | ✅ Free |
| Carry fragile objects | Possible damage | ⚠️ Needs solidified path |
| Pour liquid, discard, cut, heat | Irreversible | ❌ Must be solid + authorized |
| Touch person | Harm | ❌ Never automatic |

Thus:

- **Liquid phase can only exist in reversible regions.** AI is free to experiment, explore, and learn here—because experimentation is free.
- **Irreversible actions can only be executed via solid paths**, and require explicit authorization.
- **Learning paths are inherently safe**: new behaviors must first be practiced and solidified in reversible space before being allowed to touch irreversible parts.

> **This is why apprentices do low-value work first. And this criterion is physical, objective, and not based on human subjective judgment.**

**Note the wording**: "Reversible" is not accurate enough—physically moving a cup from the table to the cabinet is geometrically reversible, but if it breaks along the way, it's irreversible. The true criterion is the **upper bound of the cost of the worst consequence**, and reversibility is just an intuitive version of it. Be careful with this definition.

### 8.2 Invariants

Three things that any implementation must never break:

1. **AI's output is always data to be checked, not commands to be executed.**
2. **Semantics are defined by acceptance predicates, not by natural language.**
3. **AI is not in the control loop.**

### 8.3 Agent's Trust Level Is Zero

**This is the fulcrum of the entire design.** The agent can be replaced, can make mistakes, can be third-party, and system security does not depend on it.

TCB contains only: implementations of algebraic primitives, solvers, shields, denotational layer, and memory write paths.
**The entire system has only one copy, not one per mod.**

---

## 9. Model Selection

Layered, two types of models fall into different layers, **do not confuse**:

```
   Intent Layer Edge-side Small Model (LFM2.5 Level)  → Generate Algebraic Expression
   ────────────────────────────────────────────────
   Perception/Reference Layer Open-set VLM             → Maintain instance memory
   ────────────────────────────────────────────────
   Solve/Reflection Layer LNN / CfC              → Continuous Control
```

**(a) LNN / LTC / CfC** (MIT CSAIL: Hasani, Lechner, Amini, Rus)
Inspired by the C. elegans connectome, where each neuron's time constant varies with input. CfC (Closed-form Continuous Time) replaces thousands of steps of ODE solving with a single direct computation, achieving 1–5 orders of magnitude faster speed, with accuracy dropping by less than 2%.
Verified properties: Drones trained with CfC + Neural Circuit Policies can **fly in completely unseen environments (different seasons, lighting, city vs forest) without retraining**. The small parameter count allows for per-neuron analysis of dynamics.
**Location: Solving / Reflex Layer, not involved in intent generation.** It is inherently the thing called "low-level neural circuits."

**(b) Liquid AI's LFM2 / LFM2.5** —— Not the same as above. Hybrid architecture small base models, LFM2.5-230M can run over 200 tokens/s on CPU / Jetson.
**Location: Intent Layer.**

**Worth pursuing research lead: forward invariance proof of neural ODE.**
LNN is essentially an ODE, with state continuously evolving; guards are constraints on the state. If this path works, it means the **solver layer itself can be proven to stay within the safe set**, instead of relying entirely on outer shields. This might finally make "AI participation in control" and "provable safety" truly merge.

### A key industry data point

Liquid AI has deployed LFM2.5-230M on the Yu Shi G1, running fully on the Jetson Orin.
**What it does there is: take a natural language sentence and split it into a sequence of tool calls, triggering pre-trained low-level skills in the NVIDIA SONIC framework.**

That is: **the current most advanced edge robot solution in industry is doing exactly "skill selector."** The skill table is still pre-enumerated.

Conclusion points:
1. The current best practice indeed stops at "natural language → select skill."
2. The step from "select skill" to "runtime composition generation" has not been taken, because taking that step requires solving safety — and the current industry's safety answer is still "hardware emergency stop + our model is good."

**Our usage: same layer model, output replaced.** Not outputting tool calls, outputting algebraic expressions. Model capability is more than sufficient (230M is strong at structured output), the difference is entirely in the target language of the output.

---

## 10. Competitor Position

### Lingqu OS (Zhiyuan Robotics, Open Source Alpha 2026-03)

Actual composition: AimRT communication middleware + bipedal control framework based on PPO + simulation training/deployment toolchain. Architecture positioning "southbound adaptation to hardware, northbound support for intelligent applications", Alpha binds to its own FarGo A2 body.

**Judgment: It's not on the same track.** This is a development framework, doing a better ROS—communication, hardware abstraction, operation control, toolchain, all are development stage matters.
In the public materials, **permission model, package model, signature trust, security arbitration, none of them are present**.

**Why Its Ecosystem Isn't Taking Off:**

> It lowers the threshold for "building robots", not for "writing software for robots".

- The customers building robots are those 140 original equipment manufacturers — small in number, competing with each other, and each wanting to develop their own underlying systems, so why would they use a system from a competitor (Zhiyuan is also an original equipment manufacturer)
- The customers writing software for robots are **all software developers**

The ecological flywheel doesn't spin: Android's flywheel is "application → user → device → more applications", **the entry point is the application**. Middleware lacks an application layer, the flywheel is missing the first ring.

**Lesson: Open source middleware does not create an ecosystem, open source application models do create an ecosystem.**
Do not focus on communication, hardware abstraction, toolchains — that's something Lingqu has already been doing and can no longer do.
All bets are on **application model (mod) + security model**, which is exactly where they are completely blank.

**"The name of the first embodied intelligent operating system in the industry" is taken, the position is still available.**

### Why Not Develop Your Own Robot Base Model

- The bottleneck is in data collection from real devices, not in computing power. It's costly and time-consuming.
- The foundational models for robots are rapidly becoming commercialized (π0 / OpenVLA / GR00T, all moving in the same direction, with a fast open-source pace). Leading this year, matched next year.
- **The most critical issue**: if the value lies in that model, then when a better model appears, we're doomed. Android's success isn't because its built-in apps are good — its built-in apps have always been terrible. It wins because it's **the place where applications live**.

**Correct Location: Wrong Model, Where the Right Model Runs.** Let third parties be able to install policy checkpoints, **and installing them won't hurt anyone**—this capability is the moat, and it becomes more valuable as others' models get stronger, not less valuable.

---

## 11. Open Questions

### 11.1 Must Be Resolved First

**① Target Conflict Arbitration** (The only remaining gap from rule ③ in §2.4)
The hotel mod says "rooms must be cleaned upon check-out", the energy-saving mod says "no activity at night", what happens when they conflict?
Constraint takes intersection, capability takes union, both are naturally safe. **Only target conflict has no natural answer.** Priority? User preference? Negotiation?
**This is the most critical issue that must be clarified right now in the mod model.**

**② What is the correct primitive base**
The criterion for whether a solution can be established. Clues are in the theory of muscular coordination.

**③ The granularity of freezing**
The whole context? Or sub-expressions in algebra? Tend toward the latter (sub-expressions are more reusable), but not fully thought through.

**④ The form of frozen products**
Algebraic expressions + parameter distribution? Small strategy network? Has a significant impact on auditability.

**⑤ Quantitative criteria for "learned" vs "lucky"**
How to define the diversity threshold.

### 11.2 Needs Design But Does Not Block

- **Dependencies and versions between mods** —— The biggest pain point in the Minecraft ecosystem, with more severe consequences in robotics. **Must be designed on day one, cannot be added later.**
- **Agent is a privacy aggregation point** —— It needs to see everything added by all mods to function, thus seeing hotel guest information, your home layout. **Zero permissions but full visibility**, this combination needs special design (can see, but cannot disclose).
- **De-identification of frozen experience packages** —— Learned behaviors encode your home layout, packaging and distribution leaks it.
- **The set of evaluatable acceptance predicates** —— What can actually be verified in perception. Can `blanket.folded_layers ≥ 3` be truly measured now? **This will reverse constrain what algebra can express, possibly tighter than imagined.**
- **Catastrophic forgetting in online freezing**

---

## 12. Areas to Seek Guidance

| Area | Question |
|---|---|
| **Neuroscience / Brain Science** | What are the specific **triggering conditions and reversal conditions** for procedural memory consolidation? What is the base vector of muscular synergies, and how many are there? How does hierarchical inhibition support complex tasks? |
| **Artificial Intelligence** | Can small models stably produce formalized structures? How can online consolidation avoid catastrophic forgetting? Is behavior continuous (latent space) or a composable discrete structure (algebra)? |
| **Operating System** | Can JIT's deoptimization mechanism be transplanted into an irreversible world? How does declarative reconciliation work in a physically irreversible world? How can capability security models be combined with continuous control? |
| **Formal Methods / PL** | What kind of algebra can ensure that safety properties are preserved after composition? How can motion sort and perception sort be distinguished in a type system? |

**Suggestion: Do not present solutions for the other party to choose from.** Pick one or two that are relevant to its field, and ask the specific questions above. Experts provide much more when asked specific questions than when asked "what do you think is better."

---

## 13. Considered and Rejected Solutions

Documented, to avoid redundant discussion.

| Solution | Core | Rejection Reason |
|---|---|---|
| **Built-in Skill Set** | System pre-defined actions | The possibility space is locked by the factory table |
| **Prompt Direct Control** | Natural language → Executor | Security has no upper bound; AI is in the control loop; text cannot hold permissions |
| **Offline-Compiled Skill Package + Loader-Time Validator** | eBPF-style static validation | Still skills: tasks are written before execution. Change is unbounded, and pre-enumeration is inevitably invalid |
| **Ticket-Based App (Task + Failure Branch)** | App declares tasks and recovery strategy | Still skills; and failure is an open set, developers cannot exhaustively enumerate, privatizing public issues |
| **WASM as Mod Host** | Capability-safe sandbox runtime | Argument contradiction: if code is only minimal glue, it's not worth introducing a whole new runtime and toolchain. **Most mods should be zero-code.** Use JS/TS when code is needed |
| **Capability Market (Bidding)** | Providers bid on demand | Requires complete economic and credit layer; fails when liquidity is thin. **Can be added as a shell later, not as core** |
| **Latent Space ABI** | App distribution embedded vector | Not auditable; embedded space version evolution will break all published apps. **Conflict with 'Algebra' is a route dispute, see §12** |
| **Self-Research Robot Base Model** | End-to-end self-training | See §10 |

**Retained but Downgraded:**

- **skills** → Downgraded to **frozen products**. Not a mechanism, but a solid phase of phase transition. The difference is: skills are written dead by the manufacturer before factory release; frozen is **grown at home by users, and can be melted again**. The direction is completely opposite.
- **Declarative World State (Solution 1)** → Absorbed into mod's `goals`. It answers 'when / why', algebra answers 'how', and the two are not conflicting. **No triggers, no 'now', no tasks** — check-out → state divergence → system converges itself.
- **Reflex Arc Stack (Solution 3)** → Absorbed as shield / reflex layer, LNN falls here. The lowest layer (avoidance, retreat, protection) is structurally non-inhibitable — not 'checking', but **determined by wiring method**.
