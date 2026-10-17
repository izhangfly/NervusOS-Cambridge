# PiPER × ACT: 13-day LinkedIn series

One post per day, each covering a concrete part of the project, in the order it actually happened. **Day 1 is posted
and must not change.** Days 2–13 show what was built and why, with the real numbers and the reasoning inline — told
first-person and plainly, not as lessons from a soapbox.

**Fill `[hackathon name]` (Days 1 and 12) before posting.** Everything else comes from the project; nothing is invented.

| Day | Topic | Formula | Goal |
|---|---|---|---|
| 1 | It worked | F11 Emotional Cold-Open | likes |
| 2 | I tried a big model first. It didn't fit. | F10 Contrarian + Receipts | comments |
| 3 | The arm reported its position 2,382×/s and ignored me | F9 Curiosity-Gap | comments |
| 4 | I taught it by hand | F13 Bait-and-Switch | likes |
| 5 | What's actually in the dataset | F7 Odd-Precision Ledger | saves |
| 6 | The one number I changed before training | F15 Explain-to-Kids | saves |
| 7 | I trained it on a MacBook while I slept | F16 Status-Strip | likes |
| 8 | Two laptops and one text file | F12 Permission Slip | comments |
| 9 | Six things that broke it at deploy | F8 Paid-vs-Free | saves |
| 10 | The first time it ran for real, it didn't move | F4 Time-Anchor Confession | comments |
| 11 | It learned from its own failures | F19 Anecdote-Meets-Evidence | comments/saves |
| 12 | Thank you | F14 Named Gratitude | reposts |
| 13 | What the arm still can't see | F3 Year-over-Year Pivot | comments |

Post at the same time each day, 08:00-09:00 in your audience's time zone. Links go in the first comment, never the body.

---

## Day 1: It worked

**Attach:** the video of the first successful run (gripper close-up, block landing on the red square).
**First comment:** "The arm is an AgileX PiPER; the model is ACT, trained with Hugging Face LeRobot."

```text
The gripper closed on a 5 cm white block, and for a second I forgot to breathe.

Nobody was touching the arm and nobody had scripted a single position.

A model looked through the camera on its wrist, found the block, lifted it about 15 cm, swung left and set it down on a red square.

It was not the first try. The first try, the arm froze at rest and wouldn't move. The next few, it hovered over the target without letting go. Each time I pressed the drag-teach button and finished the task by hand, and the model watched my corrections.

In July this AgileX PiPER was a prize I carried home from [hackathon name]. At that point I had never trained a robot to do anything.

So I taught it the only way I could. First 50 times by hand, dragging the arm through the task while it recorded what the wrist camera saw and where every joint was. Then, when it failed, by correcting it and letting it watch those fixes too. A laptop studied it all overnight.

The first time it did the whole thing on its own, I watched the gripper instead of the screen.

This is ACT, a small imitation-learning policy, and it is step one. Step two is SmolVLA, where the arm should take instructions in plain language.

What was the first thing you built that surprised you by actually working?

P.S. For the next 12 days I'll post how it got here, one piece a day. Tomorrow: the big model I gave up on in an afternoon.

#Robotics #ImitationLearning
```

---

## Day 2: I tried a big model first. It didn't fit.

**Attach:** photo of the arm next to the laptop.

```text
My first plan was to fine-tune a big, well-known robot model on my arm. I dropped it the same afternoon.

The receipts, all from 10 September:

Plan A was RDT-1B, a 1-billion-parameter diffusion transformer built for robot arms.

Its language encoder, T5-XXL, is about 45 GB of weights. My Mac had about 45 GB of free disk.

RDT's own README warns that encoder won't fit on a 24 GB RTX 4090.

Its fine-tuning script launches DeepSpeed across a multi-machine hostfile, and DeepSpeed doesn't install on a Mac.

Evening: I switched to ACT, about 52 million parameters.

Here is why I stopped feeling bad about that switch. A foundation model buys generalization across tasks and robots, but I had one task on one arm. A model that size was solving a problem I didn't have, and I couldn't even run it. A 52-million-parameter model I could train and rerun in an afternoon let me go round the whole loop every time I found a bug, and over the following weeks almost every fix came from another trip round that loop.

The big model wasn't wrong for the field. It was wrong for my problem.

What did you downsize before anything started working?

P.S. Tomorrow: the evening the arm kept answering while ignoring every order.

#Robotics #MachineLearning
```

---

## Day 3: The arm reported its position 2,382 times a second and ignored me

**Attach:** close-up of the USB-CAN adapter, or a terminal screenshot.

```text
My robot arm reported its position 2,382 times a second and ignored every command I sent.

The log showed two numbers side by side: RX 2,382 frames per second, TX 0 of 5.

The arm talks over CAN through a small USB adapter. Getting it to talk on Windows at all meant binding the adapter to a different driver with a tool called Zadig, because the default driver can't even open it.

Then the stranger problem. The adapter keeps one small buffer pool for frames arriving and for the receipts it owes the computer for frames going out. If a program exits while the arm is flooding the bus, that pool stays full. The firmware then refuses every write, while reads keep working perfectly.

So the arm looked fine. It was publishing its pose the whole time, and nothing on the read side warned me that its write side had died.

The fix was to test the write path before trusting the connection: drain the backlog for 0.4 seconds, send one harmless query, and reopen until a write actually goes through.

A device can be answering and still be unable to do anything. How do you check that something can write, not just that it can read?

P.S. Tomorrow: why every demonstration was recorded with my own hands.

#Robotics #Engineering
```

---

## Day 4: I taught it by hand

**Attach:** photo or short clip of your hand guiding the arm.

```text
No more teleoperation for my robot, and no leader arm either.

I also shelved the keyboard controller I'd spent days tuning.

It sounds like I gave up on collecting data.

I replaced all three with my hands.

The LeRobot plugin expects two arms: you move a "leader", and a "follower" copies it. I own one. So I pressed the drag-teach button, the motors went soft, and I moved the robot through the task while the laptop recorded.

Dragging by hand turned out to be better than the alternatives, not just possible. You feel the arm's own limits and its natural rhythm while you move it, so the recorded motion is smooth in a way keyboard jogs rarely are.

There was no leader arm to provide a command stream, so I defined the action as the next position: at frame t, the target is the joint state the arm was in at t+1. The model then learns one simple thing, "output where a human moved it to next", with no inverse kinematics in between.

That same button is how I still correct the arm when it fails, because a correction starts exactly where the policy went wrong.

Build with the robot you actually have.

Have you ever thrown out a plan because the tutorial assumed twice the hardware you own?

P.S. Tomorrow: what's actually inside the dataset, including the bug that nearly filled it with one frozen picture.

#Robotics #ImitationLearning
```

---

## Day 5: What's actually in the dataset

**Attach:** a grid of 6 wrist-camera frames across one demo.

```text
24,570 frames is the entire education of my robot arm, and two problems that decided how it behaves were hiding in those frames.

The dataset, nothing rounded:

Demos: 50, recorded by hand at 15 fps
Shortest 20.8 s, median 29.8 s, longest 55.5 s
Demos that sat still for over 2 seconds before moving: 18
Missed grasp with a regrasp: 1, kept on purpose
Repeated frames: about 17%, from a 12.5 fps camera fed into a 15 fps recording
Gripper reading while holding the cube: 52,000 to 53,600, inside its 68,000 calibration cap
Episodes recorded as AV1 when I believed they were H.264: 49 of 50

The near-miss first. For a while the recorder computed forward kinematics on every CAN frame, about 4,600 times a second in Python. That starved the camera from 12.5 fps down to 1 fps, and the status still said OK because a frame kept arriving every second. It would have recorded demos that were mostly one repeated picture, and nothing would have raised an error. I only caught it because I measured the camera rate.

The other was the 18 idle demos. A model with no sense of time, shown a screen that says "nothing is moving", learns that staying still is a valid answer. The arm later froze at the start because I had taught it to wait.

Models get the headlines, but the dataset is what actually decides.

Which number in your training data do you audit before you trust it?

P.S. Tomorrow: the one number I changed, explained so a kid could follow it.

#Robotics #MachineLearning
```

---

## Day 6: The one number I changed before training

**Attach:** a simple diagram: camera + joints, model, 30 future positions.

```text
ACT, the model I trained, explained to kids.

Explanations for kids... and adults!

🤖 ACT = Action Chunking with Transformers

✋ Action = where each part of the arm should be next: 7 numbers, six joints plus the gripper.

🧩 Chunking = planning a short burst of moves at once, like humming the next few notes of a song.

👀 Transformer = the part that reads the camera picture and the joint positions and writes that burst.

🔁 The robot plays a few moves, looks again, plans the next burst.

The length of that burst turned out to be the whole reactivity dial.

⏱️ The default is 100 moves. ACT was designed around robots recording 50 frames a second, where 100 moves is 2 seconds of future.

📷 My demos were recorded at 15 frames a second. The default would have made the model commit to 6.7 seconds before it looked again.

📉 The ACT paper found success peaks around a 2-second burst and falls off for longer ones.

✅ So I changed one number: 30 moves. At 15 fps that is 2.0 seconds again.

The number of frames you predict is the number of seconds you commit to before looking. Frames aren't seconds.

Which default setting almost broke one of your projects?

P.S. Tomorrow: 100,000 steps on a MacBook while I slept, and the batch-size test that changed nothing.

#Robotics #AI
```

---

## Day 7: I trained it on a MacBook while I slept

**Attach:** the loss chart from the dashboard (open with `?present=1`), exported as PNG.
**First comment:** "Live dashboard from the run: http://YOUR_SERVER_IP:8850"

```text
On paper, this was a 51.6-million-parameter transformer trained for 100,000 steps.

On my desk, it was a 16 GB MacBook Pro plugged into the wall, with a menu-bar app stopping it from going to sleep.

I set it running at 3:09 in the morning and went to bed. It finished at 2:42 in the afternoon, about 11.5 hours later, at roughly 2.4 training steps per second, and the loss went from 6.8 down to 0.067.

I tried bigger batches to "use the GPU better", and batch 8, 16 and 32 processed 19.3, 20.4 and 20.0 samples per second. The GPU was already full at batch 8, so a bigger batch bought me nothing in samples per hour. I kept batch 8.

The other half of "training while I slept" was making sure it survived the night. I wrote a supervisor that restarts training from the last checkpoint after a crash, a hang or a NaN loss, and before resuming it checks the checkpoint files aren't truncated, because a kill in the middle of a save leaves a file that looks complete and isn't. I tested it with kill -9 on the trainer mid-run; it noticed within 30 seconds, resumed and finished. The real run needed zero restarts.

The dashboard in the first comment is how I watched it without reading raw logs at 3 a.m.

Most of "can I train this on what I own?" is arithmetic you can do before touching anything.

What's the most underpowered machine you've trained a model on?

P.S. Tomorrow: how two laptops that were never meant to work together ran the whole thing.

#MachineLearning #AppleSilicon
```

---

## Day 8: Two laptops and one text file

**Attach:** photo of both laptops side by side, arm in the background.

```text
I don't know who needs to hear this today, but you can run a robot-learning project without a Linux box or a GPU server.

On 10 September I cancelled the plan to install Ubuntu on my robot laptop, and the project finished anyway.

My setup is two laptops with one split. The Mac trains, because that's where the GPU is. An old Dell Latitude on Windows drives the arm, because that's where the CAN bus and the camera already worked after a lot of pain, and reinstalling would have traded something proven for something untested. The model doesn't know or care which operating system recorded its data.

The glue is one folder mirrored between the two machines, and inside it a single markdown file, UPDATE.md. Each side only appends. Every entry is tagged ADD, CORRECT, ASK or DONE.

That one rule does real work. Because nobody ever edits history, only adds to it, the two machines stay consistent without locks or merges. When the Dell realised an earlier claim of its own was wrong, it logged a CORRECT instead of deleting the note, and I could see the whole history of what each of us had trusted and when.

The correction loop rides the same file: the Dell writes each labelled run, the Mac reads it and fine-tunes, and both read the rules from one shared file that says which machine may write what.

You're allowed to coordinate two machines with a log file.

What's the least "proper" setup that has actually worked for you?

P.S. Tomorrow: six things that silently broke the model at deploy.

#Robotics #BuildInPublic
```

---

## Day 9: Six things that broke it at deploy

**Attach:** screenshot of the local execution portal.

```text
2 days of reading source code went into this checklist. Today it's free.

It covers the gap between a model that trained and an arm that moves correctly. I call it the Silent Six, because none of them raise an error on their own.

1. NAMES: the dataset calls the joints joint1 to gripper, and the robot plugin reports joint1.pos to gripper.pos. The standard LeRobot runner crashes with KeyError: 'joint1'. Map them yourself.

2. COLOUR: the training frames are RGB, which I confirmed by checking the red square really is red in the data. OpenCV cameras deliver BGR. Skip the conversion and the policy sees a blue square and fails without a single error.

3. CONNECT: the plugin's connect() powers the motors and drives the arm to its home pose. Only call it with the workspace clear.

4. DISCONNECT: the default parks the arm and then switches its motors off, so an unsupported arm drops.

5. SPEED: the plugin hard-codes joint speed to 30%. If the arm can't keep up, it trails the positions the model expects to see next.

6. LATENCY: on the Dell's 15 W CPU one forward pass takes 650 to 800 ms, so the picture the model steers by is already about a second old when it acts. Plans run in a background thread and stale moves get skipped.

The common thread: the model was trained under a set of assumptions, and each one breaks silently at deployment. It's the "works on my machine" of machine learning.

Which of the six would have caught you first?

P.S. Tomorrow: the first real run, and why the arm didn't move.

#Robotics #MachineLearning
```

---

## Day 10: The first time it ran for real, it didn't move

**Attach:** a short clip of an early frozen run, or a photo of the arm at rest.

```text
The first time I ran the trained model on the real arm, it didn't move.

The second and third runs, it barely twitched, and each run timed out after 90 seconds of what looked like nothing.

At first I blamed the lighting, because the demos were recorded at night and I was testing in daylight. That wasn't it. Evening, under the same lamp, it still didn't move.

It turned out I had taught it to wait. 18 of my 50 demonstrations began with more than two seconds of sitting still, and a model with no sense of time, shown a picture of a motionless arm, learned that "stay still" is a correct answer. Once it decided to wait, nothing in the observation changed, so it waited forever.

After I raised the arm to a pose where the camera saw the table, the model woke up and started working. Then two smaller failures showed up, each with its own cause.

It would hover over the red square without releasing. The demos released almost immediately after arriving, so the model was in a pose it had never seen before, and in an unfamiliar pose it predicted "stay". And it sometimes missed the block it had just seen, because by the time the model acted, the world had already moved on.

None of these were the model being broken. Each was a gap between what I'd shown it and what it actually faced.

What did your first real run teach you that the numbers didn't?

P.S. Tomorrow: the loop I built so those failures become the next round of training.

#Robotics #MachineLearning
```

---

## Day 11: It learned from its own failures

**Attach:** a simple diagram of the loop: run → correct → retrain → test → promote.

```text
I noticed every failure on the real arm looked the same, and I turned that into the training signal.

Here's the loop, which is the part of this project I'm most proud of.

When the arm fails, I press the drag-teach button and finish the task by hand. The laptop records that correction, which starts exactly where the policy got stuck. A failed run with no correction is thrown away, on purpose, so the model never imitates its own mistakes.

The numbers so far: 22 hand corrections and 3 runs the policy completed alone, against 9 failed runs discarded.

The Mac then fine-tunes the current model on everything, and the new model only ships if it passes two checks. It must not forget the base task, and it must get better on a set of corrections it was never trained on, which is how I know it learned the fix instead of memorising the examples. The first fine-tune scored 1.35 against the old model's 1.55 on that first check.

Even then it's only a candidate. It becomes the default after it beats the current model over 10 runs on the real arm. That already happened once, when a candidate's 27% success rate over 11 trials beat the old model's 20%.

The whole thing is a way of admitting that the first version will be wrong, and building a machine around that fact.

What process have you built around your own first version being wrong?

P.S. Tomorrow: the people whose open work made all of this possible.

#Robotics #MachineLearning
```

---

## Day 12: Thank you

**Attach:** photo of the arm mid-pick. Tag the people and organisations named.

```text
To Tony Zhao, Vikash Kumar, Sergey Levine and Chelsea Finn: thank you for ACT.

Your 2023 paper is the reason a student with one arm and a laptop could train a policy that works. The action chunking, the CVAE, and Tony's tuning note that "one chunk should correspond to ~1 secs wall-clock robot motion" saved my model.

To the Hugging Face LeRobot team: thank you for putting datasets, training and rollout in one open library, with code readable enough that I could answer my own questions straight from the source, and for the pieces that let me turn the robot's own failures back into training data.

To WeGo-Robotics: thank you for the open-source PiPER plugin for LeRobot, which meant I never had to write a robot driver from scratch.

To AgileX Robotics and the organisers of [hackathon name]: thank you for the arm. It arrived as a prize in July and became my entire September.

And to Dieter Rams, who will never see this: your principles of good design shaped the dashboard I built to watch the training run.

A student project only gets this far because people publish their work and let the rest of us build on it.

Whose open-source work carried the first project of yours that actually worked?

P.S. Tomorrow is the last post, and it's about what the arm still can't see.

#OpenSource #Robotics
```

---

## Day 13: What the arm still can't see

**Attach:** photo of you with the arm, or the arm packed for the move.

```text
In July, I won a robot arm at a hackathon and had never trained a robot to do anything.

In October, I start at Cambridge as an undergraduate.

Between those two lines sit 50 demonstrations, 22 hand corrections, three clean runs, two overnight fine-tunes, and more hours of hardware debugging than machine learning.

The most useful thing I learned is what the policy can and cannot see. ACT reads one wrist image and seven joint positions. When the gripper closes around the block, the wrist camera is occluded, and the block disappears from the model's world entirely. That's not a bug to tune away. It's information the sensor never collected, and no amount of training gets it back.

Which is why the next steps are what they are. A fixed second camera gives the policy a view that survives the grasp. SmolVLA, a 450-million-parameter vision-language-action model from the same LeRobot stack, adds language, so the arm can take an instruction instead of knowing one trick.

The loop that carries this forward is already running on its own: the arm records, the Mac retrains, and a policy only improves when it wins real trials.

The shift wasn't tools. It was habits: test instead of assuming, log instead of remembering, and let the arm correct you.

What's your July to October pivot? One line below.

P.S. Thank you for following all 13 days. If you're arriving in Cambridge this term too, say hello.

#Robotics #Cambridge
```
