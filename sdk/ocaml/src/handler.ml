exception Panic of string

module type S = sig
  type t

  val init : unit -> t
  val handle : Env.t -> Input.t -> t -> (t, string) result
  val snapshot : (t -> string) option
  val restore : (string -> t) option
  val invariants : (string * (t -> (unit, string) result)) list
end

type failure = { kind : string; message : string; detail : string }

(* A handler with its state hidden, so the recorder and the host can drive any
   handler alike. *)
type instance = {
  run : Env.t -> Input.t -> failure option;
  snapshot : (unit -> string) option;
  invariants : string list;
}

(* The marker message of an escaped exception (SPEC §4.5): [Panic m] is exactly
   [m]; the exceptions that carry a source location are reduced to their name,
   and the location goes to the marker data. *)
let describe = function
  | Panic m -> (m, "")
  | Assert_failure (f, l, c) -> ("Assert_failure", Printf.sprintf "at %s:%d:%d\n" f l c)
  | Match_failure (f, l, c) -> ("Match_failure", Printf.sprintf "at %s:%d:%d\n" f l c)
  | Undefined_recursive_module (f, l, c) ->
      ("Undefined_recursive_module", Printf.sprintf "at %s:%d:%d\n" f l c)
  | e -> (Printexc.to_string e, "")

let message_of_exn e = fst (describe e)

let instantiate (module H : S) ~snapshot:from =
  let ( let* ) = Result.bind in
  let* st =
    match from with
    | None -> Ok (H.init ())
    | Some s -> (
        match H.restore with
        | None -> Error "the handler has no restore function"
        | Some restore -> (
            try Ok (restore s) with e -> Error ("restoring the snapshot: " ^ message_of_exn e)))
  in
  let st = ref st in
  let check () =
    List.find_map
      (fun (name, check) ->
        match check !st with
        | Ok () -> None
        | Error detail -> Some { kind = "invariant"; message = name; detail }
        | exception e -> Some { kind = "invariant"; message = name; detail = message_of_exn e })
      H.invariants
  in
  let run env input =
    (* On Error or an exception the previous state is kept; a handler that
       mutates in place keeps those mutations, as a Go handler would. *)
    match H.handle env input !st with
    | Ok st' -> st := st'; check ()
    | Error message -> Some { kind = "error"; message; detail = "" }
    | exception e ->
        let backtrace = Printexc.get_backtrace () in
        let message, where = describe e in
        Some { kind = "panic"; message; detail = where ^ backtrace }
  in
  Ok
    {
      run;
      snapshot = Option.map (fun f () -> f !st) H.snapshot;
      invariants = List.map fst H.invariants;
    }
