module Json = Json
module Base64 = Base64
module Input = Input
module Env = Env
module Handler = Handler
module Wire = Wire
module Recorder = Recorder
module Host = Host
module Conformance = Conformance

module type HANDLER = Handler.S

exception Panic = Handler.Panic

type failure = Handler.failure = { kind : string; message : string; detail : string }

let maybe_host = Host.maybe_host
