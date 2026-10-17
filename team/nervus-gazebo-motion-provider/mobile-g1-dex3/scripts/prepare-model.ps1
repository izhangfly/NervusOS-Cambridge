[CmdletBinding()]
param(
    [ValidateNotNullOrEmpty()]
    [string]$Distro = "Nervus-Ubuntu",

    [string]$SourceRoot = "",

    [string]$DestinationRoot = ""
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$variantRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
$gazeboRoot = [System.IO.Path]::GetFullPath((Join-Path $variantRoot "..\.."))
if ([string]::IsNullOrWhiteSpace($SourceRoot)) {
    $SourceRoot = Join-Path $gazeboRoot "third_party\unitree_ros\robots\g1_description"
}
if ([string]::IsNullOrWhiteSpace($DestinationRoot)) {
    $DestinationRoot = Join-Path $variantRoot "simulation\models\nervus_mobile_g1_dex3"
}
$SourceRoot = [System.IO.Path]::GetFullPath($SourceRoot)
$DestinationRoot = [System.IO.Path]::GetFullPath($DestinationRoot)
$sourceRepository = [System.IO.Path]::GetFullPath((Join-Path $SourceRoot "..\.."))
$sourceUrdf = Join-Path $SourceRoot "g1_29dof_with_hand_rev_1_0.urdf"
$sourceLicense = Join-Path $sourceRepository "LICENSE"

$expectedSourceCommit = "daadf41ee9afce8f90fdc09a98506012691fa122"
$expectedUpperJointCount = 31
$expectedMeshCount = 35

function ConvertTo-WslPath {
    param([Parameter(Mandatory = $true)][string]$WindowsPath)

    $fullPath = [System.IO.Path]::GetFullPath($WindowsPath)
    if ($fullPath -notmatch '^([A-Za-z]):\\(.*)$') {
        throw "Only drive-letter paths can be converted to WSL paths: $fullPath"
    }
    $drive = $Matches[1].ToLowerInvariant()
    return "/mnt/$drive/$($Matches[2].Replace('\', '/'))"
}

function Add-TextElement {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Parent,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Value
    )

    $element = $Document.CreateElement($Name)
    $element.InnerText = $Value
    [void]$Parent.AppendChild($element)
    return $element
}

function Write-XmlDocument {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][string]$Path
    )

    $Document.PreserveWhitespace = $false
    $settings = New-Object System.Xml.XmlWriterSettings
    $settings.Indent = $true
    $settings.IndentChars = "  "
    $settings.NewLineChars = "`n"
    $settings.NewLineHandling = [System.Xml.NewLineHandling]::Replace
    $settings.Encoding = New-Object System.Text.UTF8Encoding($false)
    $settings.OmitXmlDeclaration = $false
    $writer = [System.Xml.XmlWriter]::Create($Path, $settings)
    try {
        $Document.Save($writer)
    } finally {
        $writer.Dispose()
    }
}

function Write-Utf8NoBom {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Content
    )

    [System.IO.File]::WriteAllText($Path, $Content, (New-Object System.Text.UTF8Encoding($false)))
}

function Add-UrdfOrigin {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Parent,
        [Parameter(Mandatory = $true)][string]$Xyz,
        [string]$Rpy = "0 0 0"
    )

    $origin = $Document.CreateElement("origin")
    $origin.SetAttribute("xyz", $Xyz)
    $origin.SetAttribute("rpy", $Rpy)
    [void]$Parent.AppendChild($origin)
}

function Add-UrdfInertial {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Link,
        [Parameter(Mandatory = $true)][double]$Mass,
        [Parameter(Mandatory = $true)][string]$Inertia,
        [string]$Origin = "0 0 0"
    )

    $culture = [System.Globalization.CultureInfo]::InvariantCulture
    $inertial = $Document.CreateElement("inertial")
    Add-UrdfOrigin $Document $inertial $Origin
    $massElement = $Document.CreateElement("mass")
    $massElement.SetAttribute("value", $Mass.ToString("0.########", $culture))
    [void]$inertial.AppendChild($massElement)
    $parts = $Inertia.Split(' ', [System.StringSplitOptions]::RemoveEmptyEntries)
    if ($parts.Count -ne 6) {
        throw "Inertia must contain ixx ixy ixz iyy iyz izz."
    }
    $inertiaElement = $Document.CreateElement("inertia")
    foreach ($index in 0..5) {
        $inertiaElement.SetAttribute(@("ixx", "ixy", "ixz", "iyy", "iyz", "izz")[$index], $parts[$index])
    }
    [void]$inertial.AppendChild($inertiaElement)
    [void]$Link.AppendChild($inertial)
}

function Add-UrdfGeometry {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Link,
        [Parameter(Mandatory = $true)][string]$ElementName,
        [Parameter(Mandatory = $true)][ValidateSet("box", "cylinder", "sphere")][string]$Shape,
        [Parameter(Mandatory = $true)][string]$Dimensions,
        [string]$Origin = "0 0 0",
        [string]$Rpy = "0 0 0",
        [string]$Material = ""
    )

    $element = $Document.CreateElement($ElementName)
    Add-UrdfOrigin $Document $element $Origin $Rpy
    $geometry = $Document.CreateElement("geometry")
    $shapeElement = $Document.CreateElement($Shape)
    $parts = $Dimensions.Split(' ', [System.StringSplitOptions]::RemoveEmptyEntries)
    switch ($Shape) {
        "box" { $shapeElement.SetAttribute("size", $Dimensions) }
        "cylinder" {
            if ($parts.Count -ne 2) { throw "Cylinder dimensions must be radius and length." }
            $shapeElement.SetAttribute("radius", $parts[0])
            $shapeElement.SetAttribute("length", $parts[1])
        }
        "sphere" { $shapeElement.SetAttribute("radius", $parts[0]) }
    }
    [void]$geometry.AppendChild($shapeElement)
    [void]$element.AppendChild($geometry)
    if ($ElementName -eq "visual" -and -not [string]::IsNullOrWhiteSpace($Material)) {
        $materialElement = $Document.CreateElement("material")
        $materialElement.SetAttribute("name", $Material)
        [void]$element.AppendChild($materialElement)
    }
    [void]$Link.AppendChild($element)
}

function Set-UrdfBoxCollision {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Link,
        [Parameter(Mandatory = $true)][string]$Size,
        [Parameter(Mandatory = $true)][string]$Origin
    )

    foreach ($collision in @($Link.SelectNodes('collision'))) {
        [void]$Link.RemoveChild($collision)
    }
    Add-UrdfGeometry $Document $Link "collision" "box" $Size $Origin
}

function Add-FixedJoint {
    param(
        [Parameter(Mandatory = $true)][System.Xml.XmlDocument]$Document,
        [Parameter(Mandatory = $true)][System.Xml.XmlElement]$Robot,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Parent,
        [Parameter(Mandatory = $true)][string]$Child,
        [Parameter(Mandatory = $true)][string]$Origin
    )

    $joint = $Document.CreateElement("joint")
    $joint.SetAttribute("name", $Name)
    $joint.SetAttribute("type", "fixed")
    Add-UrdfOrigin $Document $joint $Origin
    $parentElement = $Document.CreateElement("parent")
    $parentElement.SetAttribute("link", $Parent)
    [void]$joint.AppendChild($parentElement)
    $childElement = $Document.CreateElement("child")
    $childElement.SetAttribute("link", $Child)
    [void]$joint.AppendChild($childElement)
    [void]$Robot.AppendChild($joint)
}

foreach ($required in @($sourceUrdf, $sourceLicense)) {
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) {
        throw "Required Unitree source file is missing: $required"
    }
}

$sourceCommit = (& git -c "safe.directory=$($sourceRepository.Replace('\', '/'))" -C $sourceRepository rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $sourceCommit -ne $expectedSourceCommit) {
    throw "Expected Unitree source commit $expectedSourceCommit, found $sourceCommit."
}

[xml]$source = Get-Content -Raw -Encoding utf8 -LiteralPath $sourceUrdf
$keepLinkPattern = '^(waist|torso|logo|head|left_shoulder|left_elbow|left_wrist|left_hand|right_shoulder|right_elbow|right_wrist|right_hand)'
$keptLinks = @($source.SelectNodes('/robot/link') | Where-Object { $_.GetAttribute('name') -match $keepLinkPattern })
$keptNames = @{}
foreach ($link in $keptLinks) {
    $keptNames[$link.GetAttribute('name')] = $true
}
$keptJoints = @($source.SelectNodes('/robot/joint') | Where-Object {
    $keptNames.ContainsKey($_.SelectSingleNode('parent').GetAttribute('link')) -and
    $keptNames.ContainsKey($_.SelectSingleNode('child').GetAttribute('link'))
})
$waistJoint = $source.SelectSingleNode("/robot/joint[@name='waist_yaw_joint']")
if ($null -eq $waistJoint) {
    throw "Official G1 source has no waist_yaw_joint."
}
$keptJoints = @($waistJoint) + $keptJoints
$upperActuated = @($keptJoints | Where-Object { $_.GetAttribute('type') -in @('revolute', 'continuous') })
if ($upperActuated.Count -ne $expectedUpperJointCount) {
    throw "Expected $expectedUpperJointCount upper-body joints, found $($upperActuated.Count)."
}

$meshReferences = @(
    $keptLinks | ForEach-Object { $_.SelectNodes('.//mesh[@filename]') } |
        ForEach-Object { $_.GetAttribute('filename') } |
        Sort-Object -Unique
)
if ($meshReferences.Count -ne $expectedMeshCount) {
    throw "Expected $expectedMeshCount upper-body meshes, found $($meshReferences.Count)."
}

$simulationRoot = [System.IO.Path]::GetFullPath((Join-Path $DestinationRoot "..\.."))
$worldRoot = Join-Path $simulationRoot "worlds"
$destinationMeshes = Join-Path $DestinationRoot "meshes"
$destinationSource = Join-Path $DestinationRoot "source"
[void](New-Item -ItemType Directory -Force -Path $destinationMeshes, $destinationSource, $worldRoot)

[xml]$upper = '<?xml version="1.0"?><robot name="nervus_mobile_g1_dex3"></robot>'
$robot = $upper.DocumentElement
foreach ($material in $source.SelectNodes('/robot/material')) {
    [void]$robot.AppendChild($upper.ImportNode($material, $true))
}

$baseLink = $upper.CreateElement("link")
$baseLink.SetAttribute("name", "base_link")
Add-UrdfInertial $upper $baseLink 32.0 "0.908 0 0 1.189 0 1.691"
Add-UrdfGeometry $upper $baseLink "visual" "box" "0.65 0.55 0.16" "0 0 0" "0 0 0" "dark"
Add-UrdfGeometry $upper $baseLink "collision" "box" "0.65 0.55 0.16"
[void]$robot.AppendChild($baseLink)

$columnLink = $upper.CreateElement("link")
$columnLink.SetAttribute("name", "lift_column")
Add-UrdfInertial $upper $columnLink 10.0 "0.3283333333 0 0 0.3466666667 0 0.1083333333" "0 0 0.29"
Add-UrdfGeometry $upper $columnLink "visual" "box" "0.28 0.24 0.58" "0 0 0.29" "0 0 0" "dark"
Add-UrdfGeometry $upper $columnLink "collision" "box" "0.28 0.24 0.58" "0 0 0.29"
[void]$robot.AppendChild($columnLink)
Add-FixedJoint $upper $robot "lift_column_joint" "base_link" "lift_column" "0 0 0.08"

foreach ($side in @("left", "right")) {
    $wheelLink = $upper.CreateElement("link")
    $wheelLink.SetAttribute("name", "${side}_wheel_link")
    Add-UrdfInertial $upper $wheelLink 2.0 "0.0086 0 0 0.0086 0 0.0144"
    Add-UrdfGeometry $upper $wheelLink "visual" "cylinder" "0.12 0.06" "0 0 0" "1.5707963267948966 0 0" "dark"
    Add-UrdfGeometry $upper $wheelLink "collision" "cylinder" "0.12 0.06" "0 0 0" "1.5707963267948966 0 0"
    [void]$robot.AppendChild($wheelLink)

    $wheelJoint = $upper.CreateElement("joint")
    $wheelJoint.SetAttribute("name", "${side}_wheel_joint")
    $wheelJoint.SetAttribute("type", "continuous")
    $wheelY = if ($side -eq "left") { "0.305" } else { "-0.305" }
    # The 0.12 m wheel radius and 0.04 m support-sphere radius both place
    # their contact point at z=-0.12 in the base frame.
    Add-UrdfOrigin $upper $wheelJoint "0 $wheelY 0"
    $parentElement = $upper.CreateElement("parent")
    $parentElement.SetAttribute("link", "base_link")
    [void]$wheelJoint.AppendChild($parentElement)
    $childElement = $upper.CreateElement("child")
    $childElement.SetAttribute("link", "${side}_wheel_link")
    [void]$wheelJoint.AppendChild($childElement)
    $axisElement = $upper.CreateElement("axis")
    $axisElement.SetAttribute("xyz", "0 1 0")
    [void]$wheelJoint.AppendChild($axisElement)
    [void]$robot.AppendChild($wheelJoint)
}

foreach ($end in @("front", "rear")) {
    $casterLink = $upper.CreateElement("link")
    $casterLink.SetAttribute("name", "${end}_support_link")
    Add-UrdfInertial $upper $casterLink 0.25 "0.00016 0 0 0.00016 0 0.00016"
    Add-UrdfGeometry $upper $casterLink "visual" "sphere" "0.04" "0 0 0" "0 0 0" "dark"
    Add-UrdfGeometry $upper $casterLink "collision" "sphere" "0.04"
    [void]$robot.AppendChild($casterLink)
    $casterX = if ($end -eq "front") { "0.26" } else { "-0.26" }
    Add-FixedJoint $upper $robot "${end}_support_joint" "base_link" "${end}_support_link" "$casterX 0 -0.08"
}

foreach ($link in $keptLinks) {
    [void]$robot.AppendChild($upper.ImportNode($link, $true))
}

# Triangle-mesh finger contacts are expensive and produce irregular contact
# normals. Preserve the official visual meshes and inertias, but use bounded
# boxes for the moving phalanges in both hands.
$fingerCollisionSpecs = [ordered]@{
    'hand_index_0_link' = @('0.046 0.018 0.018', '0.023 0 0')
    'hand_index_1_link' = @('0.052 0.018 0.018', '0.026 0 0')
    'hand_middle_0_link' = @('0.046 0.018 0.018', '0.023 0 0')
    'hand_middle_1_link' = @('0.052 0.018 0.018', '0.026 0 0')
    'hand_thumb_0_link' = @('0.026 0.030 0.022', '-0.001 {0}0.010 0')
    'hand_thumb_1_link' = @('0.018 0.046 0.018', '0 {0}0.023 0')
    'hand_thumb_2_link' = @('0.018 0.052 0.018', '0 {0}0.026 0')
}
foreach ($side in @('left', 'right')) {
    $thumbSign = if ($side -eq 'left') { '-' } else { '' }
    foreach ($suffix in $fingerCollisionSpecs.Keys) {
        $linkName = "${side}_${suffix}"
        $link = $upper.SelectSingleNode("/robot/link[@name='$linkName']")
        if ($null -eq $link) {
            throw "Missing hand link while installing contact collision: $linkName"
        }
        $spec = $fingerCollisionSpecs[$suffix]
        $origin = [string]::Format($spec[1], $thumbSign)
        Set-UrdfBoxCollision $upper $link $spec[0] $origin
    }
}
foreach ($joint in $keptJoints) {
    $clone = [System.Xml.XmlElement]$upper.ImportNode($joint, $true)
    if ($clone.GetAttribute('name') -eq 'waist_yaw_joint') {
        $clone.SelectSingleNode('parent').SetAttribute('link', 'lift_column')
        $clone.SelectSingleNode('origin').SetAttribute('xyz', '0 0 0.58')
    }
    [void]$robot.AppendChild($clone)
}

foreach ($relativeMesh in $meshReferences) {
    if ([System.IO.Path]::IsPathRooted($relativeMesh) -or $relativeMesh.Contains('..')) {
        throw "Unsafe mesh reference in Unitree URDF: $relativeMesh"
    }
    $sourceMesh = Join-Path $SourceRoot $relativeMesh
    if (-not (Test-Path -LiteralPath $sourceMesh -PathType Leaf)) {
        throw "Referenced Unitree mesh is missing: $sourceMesh"
    }
    Copy-Item -Force -LiteralPath $sourceMesh -Destination $destinationMeshes
}

$upperUrdfPath = Join-Path $destinationSource "nervus_mobile_g1_dex3.urdf"
Write-XmlDocument $upper $upperUrdfPath
Copy-Item -Force -LiteralPath $sourceLicense -Destination (Join-Path $destinationSource "LICENSE.unitree_ros")

$upperUrdfWsl = ConvertTo-WslPath $upperUrdfPath
$sdfLines = & wsl.exe -d $Distro -- gz sdf -p $upperUrdfWsl
if ($LASTEXITCODE -ne 0) {
    throw "Gazebo SDFormat conversion failed with exit code $LASTEXITCODE."
}
[xml]$sdf = [string]::Join("`n", [string[]]$sdfLines)
$model = $sdf.SelectSingleNode('/sdf/model')
if ($null -eq $model) {
    throw "Gazebo conversion did not produce a model."
}
$model.SetAttribute('name', 'nervus_mobile_g1_dex3')
$static = $sdf.CreateElement('static')
$static.InnerText = 'false'
[void]$model.PrependChild($static)

$jointStatePublisher = $sdf.CreateElement('plugin')
$jointStatePublisher.SetAttribute('filename', 'gz-sim-joint-state-publisher-system')
$jointStatePublisher.SetAttribute('name', 'gz::sim::systems::JointStatePublisher')
foreach ($jointName in @('left_wheel_joint', 'right_wheel_joint') + @($upperActuated | ForEach-Object { $_.GetAttribute('name') })) {
    [void](Add-TextElement $sdf $jointStatePublisher 'joint_name' $jointName)
}
[void]$model.AppendChild($jointStatePublisher)

$culture = [System.Globalization.CultureInfo]::InvariantCulture
foreach ($joint in $upperActuated) {
    $jointName = $joint.GetAttribute('name')
    $limit = $joint.SelectSingleNode('limit')
    $velocity = [double]::Parse($limit.GetAttribute('velocity'), $culture)
    $controller = $sdf.CreateElement('plugin')
    $controller.SetAttribute('filename', 'gz-sim-joint-position-controller-system')
    $controller.SetAttribute('name', 'gz::sim::systems::JointPositionController')
    [void](Add-TextElement $sdf $controller 'joint_name' $jointName)
    [void](Add-TextElement $sdf $controller 'joint_index' '0')
    [void](Add-TextElement $sdf $controller 'initial_position' '0')
    if ($jointName -like '*hand*') {
        [void](Add-TextElement $sdf $controller 'use_velocity_commands' 'true')
        [void](Add-TextElement $sdf $controller 'cmd_max' '0.45')
        [void](Add-TextElement $sdf $controller 'cmd_min' '-0.45')
    } else {
        $speed = [Math]::Min($velocity, 1.0)
        [void](Add-TextElement $sdf $controller 'use_velocity_commands' 'true')
        [void](Add-TextElement $sdf $controller 'cmd_max' $speed.ToString('0.###', $culture))
        [void](Add-TextElement $sdf $controller 'cmd_min' (-$speed).ToString('0.###', $culture))
    }
    [void]$model.AppendChild($controller)
}

$diffDrive = $sdf.CreateElement('plugin')
$diffDrive.SetAttribute('filename', 'gz-sim-diff-drive-system')
$diffDrive.SetAttribute('name', 'gz::sim::systems::DiffDrive')
[void](Add-TextElement $sdf $diffDrive 'left_joint' 'left_wheel_joint')
[void](Add-TextElement $sdf $diffDrive 'right_joint' 'right_wheel_joint')
[void](Add-TextElement $sdf $diffDrive 'wheel_separation' '0.61')
[void](Add-TextElement $sdf $diffDrive 'wheel_radius' '0.12')
[void](Add-TextElement $sdf $diffDrive 'topic' '/model/nervus_mobile_g1_dex3/cmd_vel')
[void](Add-TextElement $sdf $diffDrive 'odom_topic' '/model/nervus_mobile_g1_dex3/wheel_odometry')
[void](Add-TextElement $sdf $diffDrive 'tf_topic' '/model/nervus_mobile_g1_dex3/wheel_tf')
[void](Add-TextElement $sdf $diffDrive 'frame_id' 'nervus_mobile_g1_dex3/odom')
[void](Add-TextElement $sdf $diffDrive 'child_frame_id' 'nervus_mobile_g1_dex3/base_link')
[void](Add-TextElement $sdf $diffDrive 'odom_publish_frequency' '50')
[void](Add-TextElement $sdf $diffDrive 'max_linear_acceleration' '0.25')
[void](Add-TextElement $sdf $diffDrive 'min_linear_acceleration' '-0.25')
[void](Add-TextElement $sdf $diffDrive 'max_angular_acceleration' '0.60')
[void](Add-TextElement $sdf $diffDrive 'min_angular_acceleration' '-0.60')
[void](Add-TextElement $sdf $diffDrive 'max_linear_velocity' '0.35')
[void](Add-TextElement $sdf $diffDrive 'min_linear_velocity' '-0.25')
[void](Add-TextElement $sdf $diffDrive 'max_angular_velocity' '0.60')
[void](Add-TextElement $sdf $diffDrive 'min_angular_velocity' '-0.60')
[void]$model.AppendChild($diffDrive)

# DiffDrive publishes planar wheel odometry on wheel_odometry. Keep the public
# odometry topic separate and derive it from the actual model pose so consumers
# receive roll, pitch, vertical displacement and full 3D twist as well.
$odometry = $sdf.CreateElement('plugin')
$odometry.SetAttribute('filename', 'gz-sim-odometry-publisher-system')
$odometry.SetAttribute('name', 'gz::sim::systems::OdometryPublisher')
[void](Add-TextElement $sdf $odometry 'dimensions' '3')
[void](Add-TextElement $sdf $odometry 'odom_publish_frequency' '50')
[void](Add-TextElement $sdf $odometry 'odom_topic' '/model/nervus_mobile_g1_dex3/odometry')
[void](Add-TextElement $sdf $odometry 'tf_topic' '/model/nervus_mobile_g1_dex3/tf')
[void](Add-TextElement $sdf $odometry 'odom_frame' 'nervus_mobile_g1_dex3/odom')
[void](Add-TextElement $sdf $odometry 'robot_base_frame' 'nervus_mobile_g1_dex3/base_link')
[void]$model.AppendChild($odometry)

foreach ($linkName in @('left_wheel_link', 'right_wheel_link')) {
    foreach ($collision in @($model.SelectNodes("link[@name='$linkName']/collision"))) {
        $surface = $sdf.CreateElement('surface')
        $friction = $sdf.CreateElement('friction')
        $ode = $sdf.CreateElement('ode')
        [void](Add-TextElement $sdf $ode 'mu' '2.0')
        [void](Add-TextElement $sdf $ode 'mu2' '2.0')
        [void]$friction.AppendChild($ode)
        [void]$surface.AppendChild($friction)
        [void]$collision.AppendChild($surface)
    }
}
foreach ($linkName in @('front_support_link', 'rear_support_link')) {
    foreach ($collision in @($model.SelectNodes("link[@name='$linkName']/collision"))) {
        $surface = $sdf.CreateElement('surface')
        $friction = $sdf.CreateElement('friction')
        $ode = $sdf.CreateElement('ode')
        [void](Add-TextElement $sdf $ode 'mu' '0.02')
        [void](Add-TextElement $sdf $ode 'mu2' '0.02')
        [void]$friction.AppendChild($ode)
        [void]$surface.AppendChild($friction)
        [void]$collision.AppendChild($surface)
    }
}
foreach ($collision in @($model.SelectNodes("link[starts-with(@name,'left_hand_') or starts-with(@name,'right_hand_')]/collision"))) {
    $surface = $sdf.CreateElement('surface')
    $friction = $sdf.CreateElement('friction')
    $ode = $sdf.CreateElement('ode')
    [void](Add-TextElement $sdf $ode 'mu' '5.0')
    [void](Add-TextElement $sdf $ode 'mu2' '5.0')
    [void]$friction.AppendChild($ode)
    [void]$surface.AppendChild($friction)
    [void]$collision.AppendChild($surface)
}

Write-XmlDocument $sdf (Join-Path $DestinationRoot 'model.sdf')

$modelConfig = @"
<?xml version="1.0"?>
<model>
  <name>Nervus Mobile G1 Dex3</name>
  <version>0.1.0</version>
  <sdf version="1.12">model.sdf</sdf>
  <author>
    <name>Unitree Robotics; Nervus-OS Jetty integration</name>
  </author>
  <description>Heavy differential-drive base with the official G1 waist, torso, dual seven-DoF arms and dual Dex3 hands. Motion commands and three-dimensional ground-truth odometry use Gazebo Jetty native systems.</description>
</model>
"@
Write-Utf8NoBom (Join-Path $DestinationRoot 'model.config') ($modelConfig.Trim() + "`n")

$provenance = [ordered]@{
    upstream = 'https://github.com/unitreerobotics/unitree_ros.git'
    commit = $sourceCommit
    source = 'robots/g1_description/g1_29dof_with_hand_rev_1_0.urdf'
    license = 'BSD-3-Clause'
    generated_model = 'nervus_mobile_g1_dex3'
    upper_body_joint_count = $upperActuated.Count
    wheel_joint_count = 2
    copied_mesh_count = $meshReferences.Count
    base_drive = 'gazebo_jetty_diff_drive'
    command_topic = '/model/nervus_mobile_g1_dex3/cmd_vel'
    command_message_type = 'gz.msgs.Twist'
    wheel_odometry_topic = '/model/nervus_mobile_g1_dex3/wheel_odometry'
    odometry_3d_topic = '/model/nervus_mobile_g1_dex3/odometry'
    wheel_separation_m = 0.61
    wheel_radius_m = 0.12
    max_linear_velocity_mps = 0.35
    max_reverse_velocity_mps = 0.25
    max_angular_velocity_radps = 0.60
    controller_mode = 'bounded_joint_position_velocity_command'
}
Write-Utf8NoBom (Join-Path $DestinationRoot 'SOURCE.json') (($provenance | ConvertTo-Json) + "`n")

$world = @"
<?xml version="1.0"?>
<sdf version="1.12">
  <world name="nervus_mobile_g1_dex3">
    <physics name="1ms" type="ignored">
      <max_step_size>0.001</max_step_size>
      <real_time_factor>1.0</real_time_factor>
    </physics>
    <plugin filename="gz-sim-physics-system" name="gz::sim::systems::Physics">
      <engine><filename>gz-physics-dartsim-plugin</filename></engine>
    </plugin>
    <plugin filename="gz-sim-user-commands-system" name="gz::sim::systems::UserCommands"/>
    <plugin filename="gz-sim-scene-broadcaster-system" name="gz::sim::systems::SceneBroadcaster"/>
    <gravity>0 0 -9.8</gravity>
    <scene>
      <ambient>0.65 0.65 0.65</ambient>
      <background>0.72 0.82 0.92</background>
      <grid>true</grid>
    </scene>
    <light type="directional" name="sun">
      <cast_shadows>false</cast_shadows>
      <pose>0 0 10 0 0 0</pose>
      <diffuse>0.9 0.9 0.9 1</diffuse>
      <direction>-0.5 0.3 -1</direction>
    </light>
    <model name="ground_plane">
      <static>true</static>
      <link name="ground_link">
        <collision name="ground_collision">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="ground_visual">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
        </visual>
      </link>
    </model>
    <include>
      <name>nervus_mobile_g1_dex3</name>
      <uri>model://nervus_mobile_g1_dex3</uri>
      <pose>0 0 0.12 0 0 0</pose>
    </include>
  </world>
</sdf>
"@
Write-Utf8NoBom (Join-Path $worldRoot 'nervus_mobile_g1_dex3.sdf') ($world.Trim() + "`n")

$graspWorld = @"
<?xml version="1.0"?>
<sdf version="1.12">
  <world name="nervus_mobile_g1_dex3_grasp">
    <physics name="1ms" type="ignored">
      <max_step_size>0.001</max_step_size>
      <real_time_factor>1.0</real_time_factor>
    </physics>
    <plugin filename="gz-sim-physics-system" name="gz::sim::systems::Physics">
      <engine><filename>gz-physics-dartsim-plugin</filename></engine>
    </plugin>
    <plugin filename="gz-sim-user-commands-system" name="gz::sim::systems::UserCommands"/>
    <plugin filename="gz-sim-scene-broadcaster-system" name="gz::sim::systems::SceneBroadcaster"/>
    <plugin filename="gz-sim-contact-system" name="gz::sim::systems::Contact"/>
    <gravity>0 0 -9.8</gravity>
    <scene>
      <ambient>0.65 0.65 0.65</ambient>
      <background>0.72 0.82 0.92</background>
      <grid>true</grid>
    </scene>
    <light type="directional" name="sun">
      <cast_shadows>false</cast_shadows>
      <pose>0 0 10 0 0 0</pose>
      <diffuse>0.9 0.9 0.9 1</diffuse>
      <direction>-0.5 0.3 -1</direction>
    </light>
    <model name="ground_plane">
      <static>true</static>
      <link name="ground_link">
        <collision name="ground_collision">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="ground_visual">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
        </visual>
      </link>
    </model>
    <model name="grasp_pedestal">
      <static>true</static>
      <pose>0.335 0.090 0.520 0 0 0</pose>
      <link name="pedestal_link">
        <collision name="pedestal_collision">
          <geometry><box><size>0.012 0.012 0.600</size></box></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <collision name="pedestal_top_collision">
          <pose>0 0 0.295 0 0 0</pose>
          <geometry><box><size>0.180 0.140 0.010</size></box></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="pedestal_visual">
          <geometry><box><size>0.012 0.012 0.600</size></box></geometry>
          <material><diffuse>0.25 0.28 0.32 1</diffuse><ambient>0.25 0.28 0.32 1</ambient></material>
        </visual>
        <visual name="pedestal_top_visual">
          <pose>0 0 0.295 0 0 0</pose>
          <geometry><box><size>0.180 0.140 0.010</size></box></geometry>
          <material><diffuse>0.32 0.35 0.40 1</diffuse><ambient>0.32 0.35 0.40 1</ambient></material>
        </visual>
      </link>
    </model>
    <model name="nervus_grasp_block">
      <static>false</static>
      <allow_auto_disable>false</allow_auto_disable>
      <pose>0.335 0.090 0.875 0 0 0</pose>
      <link name="object_link">
        <inertial>
          <mass>0.05</mass>
          <inertia><ixx>0.00004417</ixx><iyy>0.00004417</iyy><izz>0.00002083</izz></inertia>
        </inertial>
        <collision name="object_collision">
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <surface><friction><ode><mu>5.0</mu><mu2>5.0</mu2></ode></friction></surface>
        </collision>
        <visual name="object_visual">
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <material><diffuse>0.85 0.12 0.08 1</diffuse><ambient>0.45 0.06 0.04 1</ambient></material>
        </visual>
        <sensor name="object_contacts" type="contact">
          <contact>
            <collision>object_collision</collision>
            <topic>/nervus/grasp/object_contacts</topic>
          </contact>
          <always_on>true</always_on>
          <update_rate>100</update_rate>
        </sensor>
      </link>
    </model>
    <include>
      <name>nervus_mobile_g1_dex3</name>
      <uri>model://nervus_mobile_g1_dex3</uri>
      <pose>0 0 0.12 0 0 0</pose>
    </include>
  </world>
</sdf>
"@
Write-Utf8NoBom (Join-Path $worldRoot 'nervus_mobile_g1_dex3_grasp.sdf') ($graspWorld.Trim() + "`n")

$bimanualWorld = @"
<?xml version="1.0"?>
<sdf version="1.12">
  <world name="nervus_mobile_g1_dex3_bimanual">
    <physics name="1ms" type="ignored">
      <max_step_size>0.001</max_step_size>
      <real_time_factor>1.0</real_time_factor>
    </physics>
    <plugin filename="gz-sim-physics-system" name="gz::sim::systems::Physics">
      <engine><filename>gz-physics-dartsim-plugin</filename></engine>
    </plugin>
    <plugin filename="gz-sim-user-commands-system" name="gz::sim::systems::UserCommands"/>
    <plugin filename="gz-sim-scene-broadcaster-system" name="gz::sim::systems::SceneBroadcaster"/>
    <plugin filename="gz-sim-contact-system" name="gz::sim::systems::Contact"/>
    <gravity>0 0 -9.8</gravity>
    <scene>
      <ambient>0.65 0.65 0.65</ambient>
      <background>0.72 0.82 0.92</background>
      <grid>true</grid>
    </scene>
    <light type="directional" name="sun">
      <cast_shadows>false</cast_shadows>
      <pose>0 0 10 0 0 0</pose>
      <diffuse>0.9 0.9 0.9 1</diffuse>
      <direction>-0.5 0.3 -1</direction>
    </light>
    <model name="ground_plane">
      <static>true</static>
      <link name="ground_link">
        <collision name="ground_collision">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="ground_visual">
          <geometry><plane><normal>0 0 1</normal><size>20 20</size></plane></geometry>
        </visual>
      </link>
    </model>
    <model name="source_support">
      <static>true</static>
      <pose>0.335 0 0.815 0 0 0</pose>
      <link name="support_link">
        <collision name="support_collision">
          <geometry><box><size>0.180 0.360 0.010</size></box></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="support_visual">
          <geometry><box><size>0.180 0.360 0.010</size></box></geometry>
          <material><diffuse>0.25 0.28 0.32 1</diffuse><ambient>0.25 0.28 0.32 1</ambient></material>
        </visual>
      </link>
    </model>
    <model name="destination_support">
      <static>true</static>
      <pose>0.535 0 0.815 0 0 0</pose>
      <link name="support_link">
        <collision name="support_collision">
          <geometry><box><size>0.180 0.360 0.010</size></box></geometry>
          <surface><friction><ode><mu>1.0</mu><mu2>1.0</mu2></ode></friction></surface>
        </collision>
        <visual name="support_visual">
          <geometry><box><size>0.180 0.360 0.010</size></box></geometry>
          <material><diffuse>0.18 0.42 0.24 1</diffuse><ambient>0.18 0.42 0.24 1</ambient></material>
        </visual>
      </link>
    </model>
    <model name="nervus_bimanual_payload">
      <static>false</static>
      <allow_auto_disable>false</allow_auto_disable>
      <pose>0.335 0 0.875 0 0 0</pose>
      <link name="payload_link">
        <inertial>
          <mass>0.15</mass>
          <inertia><ixx>0.0007625</ixx><iyy>0.0001325</iyy><izz>0.0006925</izz></inertia>
        </inertial>
        <collision name="left_handle_collision">
          <pose>0 0.090 0 0 0 0</pose>
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <surface><friction><ode><mu>5.0</mu><mu2>5.0</mu2></ode></friction></surface>
        </collision>
        <collision name="right_handle_collision">
          <pose>0 -0.090 0 0 0 0</pose>
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <surface><friction><ode><mu>5.0</mu><mu2>5.0</mu2></ode></friction></surface>
        </collision>
        <collision name="bridge_collision">
          <pose>0 0 0.020 0 0 0</pose>
          <geometry><box><size>0.035 0.130 0.025</size></box></geometry>
          <surface><friction><ode><mu>2.0</mu><mu2>2.0</mu2></ode></friction></surface>
        </collision>
        <visual name="left_handle_visual">
          <pose>0 0.090 0 0 0 0</pose>
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <material><diffuse>0.86 0.14 0.09 1</diffuse><ambient>0.45 0.07 0.04 1</ambient></material>
        </visual>
        <visual name="right_handle_visual">
          <pose>0 -0.090 0 0 0 0</pose>
          <geometry><box><size>0.050 0.050 0.090</size></box></geometry>
          <material><diffuse>0.86 0.14 0.09 1</diffuse><ambient>0.45 0.07 0.04 1</ambient></material>
        </visual>
        <visual name="bridge_visual">
          <pose>0 0 0.020 0 0 0</pose>
          <geometry><box><size>0.035 0.130 0.025</size></box></geometry>
          <material><diffuse>0.95 0.62 0.08 1</diffuse><ambient>0.48 0.31 0.04 1</ambient></material>
        </visual>
        <sensor name="left_handle_contacts" type="contact">
          <contact>
            <collision>left_handle_collision</collision>
            <topic>/nervus/bimanual/left_contacts</topic>
          </contact>
          <always_on>true</always_on>
          <update_rate>100</update_rate>
        </sensor>
        <sensor name="right_handle_contacts" type="contact">
          <contact>
            <collision>right_handle_collision</collision>
            <topic>/nervus/bimanual/right_contacts</topic>
          </contact>
          <always_on>true</always_on>
          <update_rate>100</update_rate>
        </sensor>
      </link>
    </model>
    <include>
      <name>nervus_mobile_g1_dex3</name>
      <uri>model://nervus_mobile_g1_dex3</uri>
      <pose>0 0 0.12 0 0 0</pose>
    </include>
  </world>
</sdf>
"@
Write-Utf8NoBom (Join-Path $worldRoot 'nervus_mobile_g1_dex3_bimanual.sdf') ($bimanualWorld.Trim() + "`n")

Write-Host "Prepared mobile G1 Dex3 model at $DestinationRoot"
Write-Host "Source commit: $sourceCommit"
Write-Host "Upper-body joints: $($upperActuated.Count); copied meshes: $($meshReferences.Count)"
