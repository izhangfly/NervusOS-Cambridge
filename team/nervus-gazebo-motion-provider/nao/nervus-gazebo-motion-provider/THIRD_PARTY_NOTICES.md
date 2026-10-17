# Third-Party Notices

## Gazebo Msgs protobuf wire schema

`proto/gz/msgs/nervus_subset.proto` and its generated Go output contain a reduced,
wire-compatible subset of message definitions from `gazebosim/gz-msgs`.

- Upstream: https://github.com/gazebosim/gz-msgs
- Copyright: Open Source Robotics Foundation
- License: Apache License 2.0

The installed Gazebo Msgs 12 definitions under `/usr/share/gz/gz-msgs/protos` are the version
reference used by this repository. The subset omits fields that the Nervus Gazebo Motion Provider
does not inspect; protobuf unknown-field compatibility preserves interoperability with the full
upstream messages. Inclusion of a wire schema does not declare a Nervus sensor capability.
