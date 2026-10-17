# ADR-0002: Architecture Document Modification

- Status: Creating
- Date: 2026-09-01
- Decision Owners: Nervus-OS Core Team
- Writers: gohj99

This ADR is used to describe modifications to the architecture document
Corresponding architecture document version: [Nervus-OS Architecture](https://github.com/Nervus-OS/Nervus-Console/blob/8af98897e20f1068f7b18139da4732b08097fd7d/architect/NervusOS-System-Architecture-Document.md)

TODO List:
1, Notification feature
2, NSDB(Nervus Debug Bridge)
3, OOBE(out-of-box experience) interface for first boot
4, System update
5, App should be able to open other apps and allow passing parameters

Issues:

According to
Problem Number
Paragraph Number
Problem Level
Problem

01
0
p2
- NervusOS hardware form should be various forms, not a single hardware box, which does not conform to the general definition
- Remove "instead of directly loading into the robot body", which does not conform to the general definition

02
1.1.2
p5
The core and fundamental implementation of NervusOS function is a grammatically incorrect sentence, please revise

03
2
p3
6 I think init can directly supervise the nervud process, no need for additional supervisor process to occupy resources

04
2.2
p2
1 Add: must check whether the UID has permission to send

05
2.3
p1
- Only one display strategy is allowed to be effective at the same time, change to Only one unique display focus is allowed
- The display strategy is defined by the configuration file when the system starts for the first time, if in headless mode, it silently falls back to remote display. Switching to native display at this time is an invalid command, change to The native display strategy is defined by the configuration file, if the configuration file does not define output to HDMI and other video output interfaces, the display focus is only remote display
- Switching related interfaces for native display, change to Display focus switching related interfaces

06
2.3.0.1
p0
- displayd program is developed in C language
- displayd is written in Go language
Conflict, need to be corrected

07
2.3.2.1
p1
Need to supplement specific information about streaming transmission rendering, including package definition, fields, etc.

08
3.0.1
p1
Add Component ID as a sub-package of the component package, then the package must be the prefix of the Component ID, if it is not the prefix of the Component ID, it is considered as a shared service of different packages

09
3.0.1.1
p2
And version number is less than or equal to the installed package, change to And version number is less than the installed package

10
3.9.1.5
p1
id is usually package ID + component name, change to If the component is a sub-package of the component package, it must be component ID + component name, if it is not the prefix of the component ID, it is considered as a shared service of different packages

11
3.0.22
p5
Duplicate with 4.1 permission classification

12
3.1.1.1.1
p0
Its parameters are App object and start parameters
The document does not explain how to pass in the start parameters

13
3.1.1.3.8
p1
Changes to input content need to allow callback

14
3.1.1.3.9
p1
Changes to input content need to allow callback

15
3.1.1.4
p1
Display environment change, change to Display focus change

16
3.1.2.3.2
p2
Is there a need for Hello and HelloAck? Need to discuss. I think it is unnecessary

17
3.1.2.3.4
p3
Event ID adopts the reverse domain format of `com.example.event`, change to Event ID adopts the reverse domain format of `component ID.event.xxxx`

18
3.1.2.3.4
p1
- Add: check who is currently subscribed to this event
- PublishEvent add field to define who can receive the event (only sub-package or public)

19
3.1.2.5
p2
Virtual service name changed to nervud.system.server

20
3.2.0
p2
- Component registry is a data structure maintained by the NervusOS kernel, change to Component registry is a data structure maintained by the NervusOS kernel, only the kernel (root) can access
- The kernel exposes a virtual service to reload the component registry, its ID is `keep.nervud.service`, method is `reloadComponentRegistry`. This method has no parameters, returns success or failure. I think it is unnecessary
- The working directory of the component is the directory where the component code is stored. However, this directory is read-only for the component, change to The working directory of the component is the directory where the component code is stored. This directory is read-only for the component
- Component registry is a file, change to Component registry is a file, only the kernel (root) can access

21
3.2.1
p1
After creating the directory, the package manager will install the package and decompress it directly into this directory, no longer wrapping another directory. Then call the virtual service provided by the kernel to register the component. I think it should be decompressed into a tmp temporary directory, then copied to the corresponding directory to prevent attacks through compressed packages

22
4.2
p1
AddedPermission struct add SystemPermission field

23
4.5
p3
All system permissions are changed to nervus.system.perm.xxxx
