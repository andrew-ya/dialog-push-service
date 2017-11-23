import com.trueaccord.scalapb.compiler.Version.scalapbVersion
import im.dlg.DialogHouseRules

organization := "im.dlg"

name := "dialog-push-service"

version := "0.0.6.1"

scalaVersion := "2.12.4"

libraryDependencies ++= Seq(
  "io.grpc" % "grpc-netty" % "1.6.1",
  "com.trueaccord.scalapb" %% "scalapb-runtime" % scalapbVersion % "protobuf",
  "com.trueaccord.scalapb" %% "scalapb-runtime-grpc" % scalapbVersion
)

PB.targets in Compile := Seq(
  scalapb.gen(singleLineToString = true) → (sourceManaged in Compile).value
)

licenses += ("Apache-2.0", url(
  "https://www.apache.org/licenses/LICENSE-2.0.html"))

publishMavenStyle := true

bintrayOrganization := Some("dialog")

bintrayRepository := "dialog"

bintrayOmitLicense := true

DialogHouseRules.defaultDialogSettings
