type t = {
  clock : unit -> int64;
  rand : int -> string;
  query : gateway:string -> string -> (string, string) result;
  config : string -> string option;
  emit : local:bool -> sink:string -> string -> unit;
}

let now_ns e = e.clock ()
let random e n = e.rand n
let query e ~gateway request = e.query ~gateway request
let config e key = e.config key
let emit ?(local = false) e ~sink data = e.emit ~local ~sink data
