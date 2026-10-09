(* The SDK half of the flight recorder pipe (SPEC §3.6, §10).

   A [t] is for one thread: call [step], [flush] and [close] from the same one.
   Only the control-stream thread runs alongside. *)

exception Recorder_error of string

type output = { sink : string; data : string; local : bool }
type gateway = { local : bool; call : string -> (string, string) result }

let producer = "kavach-ocaml/0.1.0"
let runtime () = "ocaml-" ^ Sys.ocaml_version

type t = {
  inst : Handler.instance;
  snapshots : bool;
  deliver : output list -> unit;
  gateways : string -> gateway option;
  config : string -> string option;
  config_source : string;
  clock : unit -> int64;
  random : int -> string;
  on_fixture : Json.t -> unit;
  close_timeout : float;
  mu : Mutex.t;  (* guards the mutable fields the control thread also touches *)
  active : bool Atomic.t;
  closing : bool Atomic.t;
  mutable closed : bool;
  mutable ready : bool;
  mutable closed_ack : bool;
  mutable durable : int;
  mutable flushes : int;
  mutable snapshot_requested : bool;
  mutable file : string option;
  mutable pid : int option;
  mutable to_rec : Unix.file_descr option;
  mutable thread : Thread.t option;
  mutable buf : Buffer.t;
  mutable outputs : output list;  (* newest first *)
}

let log fmt = Printf.ksprintf (fun s -> prerr_endline ("kavach: " ^ s)) fmt
let locked t f = Mutex.lock t.mu; Fun.protect ~finally:(fun () -> Mutex.unlock t.mu) f

let fail t reason =
  if Atomic.exchange t.active false && not (Atomic.get t.closing) then
    log "%s; recording has stopped and steps run unrecorded" reason

(* The stdlib has no timed Condition.wait, so waits poll every few ms. *)
let wait_until t ~timeout cond =
  let deadline = Unix.gettimeofday () +. timeout in
  let rec go () =
    if locked t cond then true
    else if Unix.gettimeofday () >= deadline then false
    else (Thread.delay 0.002; go ())
  in
  go ()

let rec write_all fd b off len =
  if len > 0 then
    match Unix.write fd b off len with
    | n -> write_all fd b (off + n) (len - n)
    | exception Unix.Unix_error (Unix.EINTR, _, _) -> write_all fd b off len

let write t s =
  match t.to_rec with
  | Some fd when Atomic.get t.active && s <> "" -> (
      try write_all fd (Bytes.unsafe_of_string s) 0 (String.length s)
      with Unix.Unix_error (e, _, _) ->
        fail t (Printf.sprintf "could not write to the recorder: %s" (Unix.error_message e)))
  | _ -> ()

let on_control t msg =
  let str k = match Json.member k msg with Some (Json.String s) -> s | _ -> "" in
  match str "t" with
  | "ready" -> locked t (fun () -> t.file <- Some (str "file"); t.ready <- true)
  | "snapshot_request" -> locked t (fun () -> t.snapshot_requested <- true)
  | "durable" -> locked t (fun () -> t.durable <- t.durable + 1)
  | "fixture" ->
      log "wrote fixture %s (input seq %s, %s)" (str "file") (str "seq")
        (match Json.member "failure" msg with Some j -> Json.to_string j | None -> "");
      (try t.on_fixture msg with e -> log "on_fixture callback failed: %s" (Printexc.to_string e))
  | "error" ->
      let fatal = Json.member "fatal" msg = Some (Json.Bool true) in
      log "recorder %s: %s" (if fatal then "fatal error" else "error") (str "message");
      if fatal then fail t ("the recorder reported a fatal error: " ^ str "message")
  | "closed" -> locked t (fun () -> t.closed_ack <- true)
  | _ -> ()

let control_loop t ic =
  (try
     while true do
       let line = input_line ic in
       match Json.parse line with
       | msg -> on_control t msg
       | exception Json.Parse_error _ -> log "unreadable message from the recorder: %s" line
     done
   with End_of_file | Sys_error _ -> ());
  fail t "the recorder exited unexpectedly";
  locked t (fun () -> t.closed_ack <- true; t.ready <- true)

let find_command command =
  match command with
  | Some c -> c
  | None -> (
      match Sys.getenv_opt "KAVACH_RECORDER" with
      | Some p when p <> "" -> [| p |]
      | _ -> [| "kavach-recorder" |])

(* The environment is passed through unchanged (§10.1). *)
let spawn t cmd =
  let in_r, in_w = Unix.pipe ~cloexec:true () in
  let out_r, out_w = Unix.pipe ~cloexec:true () in
  let pid =
    try Unix.create_process cmd.(0) cmd in_r out_w Unix.stderr
    with e -> List.iter Unix.close [ in_r; in_w; out_r; out_w ]; raise e
  in
  Unix.close in_r;
  Unix.close out_w;
  t.pid <- Some pid;
  t.to_rec <- Some in_w;
  Atomic.set t.active true;
  let ic = Unix.in_channel_of_descr out_r in
  t.thread <- Some (Thread.create (control_loop t) ic)

let reap t ~timeout =
  match t.pid with
  | None -> ()
  | Some pid ->
      let deadline = Unix.gettimeofday () +. timeout in
      let rec go () =
        match Unix.waitpid [ Unix.WNOHANG ] pid with
        | 0, _ when Unix.gettimeofday () < deadline -> Thread.delay 0.005; go ()
        | 0, _ -> log "the recorder is still running after close"
        | _ -> t.pid <- None
        | exception Unix.Unix_error (Unix.EINTR, _, _) -> go ()
        | exception Unix.Unix_error _ -> t.pid <- None
      in
      go ()

let close_pipe t =
  Option.iter (fun fd -> try Unix.close fd with Unix.Unix_error _ -> ()) t.to_rec;
  t.to_rec <- None

let kill t =
  Atomic.set t.active false;
  Option.iter (fun pid -> try Unix.kill pid Sys.sigkill with Unix.Unix_error _ -> ()) t.pid;
  close_pipe t;
  reap t ~timeout:2.

let close t =
  if not t.closed then begin
    t.closed <- true;
    if t.pid <> None then begin
      if Atomic.get t.active then begin
        Atomic.set t.closing true;
        write t Wire.close;
        if not (wait_until t ~timeout:t.close_timeout (fun () -> t.closed_ack)) then
          log "the recorder did not answer close within %.0fs" t.close_timeout
      end;
      Atomic.set t.closing true;
      Atomic.set t.active false;
      close_pipe t;
      reap t ~timeout:2.;
      if t.pid = None then Option.iter Thread.join t.thread
    end
  end

let rec_ t frame = if Atomic.get t.active then Buffer.add_string t.buf frame

let to_ns () = Int64.of_float (Unix.gettimeofday () *. 1e9)  (* ~250ns granularity *)

let urandom =
  lazy (open_in_bin "/dev/urandom")

let os_random n = really_input_string (Lazy.force urandom) n

let record_env t : Env.t =
  {
    clock = (fun () -> let ns = t.clock () in rec_ t (Wire.clock ns); ns);
    rand =
      (fun n ->
        if n <= 0 then ""
        else (
          let d = t.random n in
          rec_ t (Wire.rand d);
          d));
    query =
      (fun ~gateway request ->
        let local, result =
          match t.gateways gateway with
          | Some g -> (g.local, try g.call request with e -> Error (Handler.message_of_exn e))
          | None -> (false, Error ("no gateway named " ^ gateway))
        in
        let response, error = match result with Ok r -> (r, "") | Error e -> ("", e) in
        rec_ t (Wire.gateway ~name:gateway ~request ~response ~error ~local);
        result);
    config =
      (fun key ->
        let v = t.config key in
        rec_ t (Wire.config ~key ~value:v ~source:t.config_source);
        v);
    emit =
      (fun ~local ~sink data ->
        t.outputs <- { sink; data; local } :: t.outputs;
        rec_ t (Wire.output ~sink ~local data));
  }

let create ?recorder_command ?(required = false) ?snapshot ?snapshots ?(deliver = ignore)
    ?(gateways = fun _ -> None) ?config ?config_source ?(flags = fun () -> []) ?handler_id ?dir
    ?compression ?level ?block_bytes ?flush_ms ?segment_bytes ?segment_seconds ?retain_segments
    ?(secret_keys = []) ?(on_fixture = ignore) ?(clock = to_ns) ?(random = os_random)
    ?(ready_timeout = 10.) ?(close_timeout = 10.) ~service handler =
  let module H = (val handler : Handler.S) in
  let inst =
    match Handler.instantiate handler ~snapshot with
    | Ok i -> i
    | Error m -> invalid_arg ("Kavach.Recorder.create: " ^ m)
  in
  let can_snapshot = Option.is_some inst.snapshot && Option.is_some H.restore in
  let snapshots = Option.value snapshots ~default:can_snapshot in
  if snapshots && not can_snapshot then
    invalid_arg "Kavach.Recorder.create: snapshots need a handler with snapshot and restore";
  let t =
    {
      inst; snapshots; deliver; gateways;
      config = Option.value config ~default:Sys.getenv_opt;
      config_source = Option.value config_source ~default:(if config = None then "env" else "config");
      clock; random; on_fixture; close_timeout;
      mu = Mutex.create ();
      active = Atomic.make false; closing = Atomic.make false;
      closed = false; ready = false; closed_ack = false; durable = 0; flushes = 0;
      snapshot_requested = false; file = None; pid = None; to_rec = None; thread = None;
      buf = Buffer.create 256; outputs = [];
    }
  in
  if Sys.os_type = "Unix" then Sys.set_signal Sys.sigpipe Sys.Signal_ignore;
  let open_obj =
    let opt k f = function Some v -> [ (k, f v) ] | None -> [] in
    let int v = Json.Int v in
    Json.Obj
      ([ ("protocol", Json.Int 1); ("service", Json.String service);
         ("start", Json.String (if snapshot = None then "genesis" else "snapshot"));
         ("producer", Json.String producer); ("snapshots", Json.Bool snapshots) ]
      @ opt "handler" (fun s -> Json.String s) handler_id
      @ opt "dir" (fun s -> Json.String s) dir
      @ opt "compression" (fun s -> Json.String s) compression
      @ opt "level" int level @ opt "block_bytes" int block_bytes @ opt "flush_ms" int flush_ms
      @ opt "segment_bytes" int segment_bytes @ opt "segment_seconds" int segment_seconds
      @ opt "retain_segments" int retain_segments
      @ if secret_keys = [] then [] else [ ("secret_keys", Json.List (List.map (fun s -> Json.String s) secret_keys)) ])
  in
  (try
     spawn t (find_command recorder_command);
     write t (Wire.open_ (Json.to_string open_obj));
     write t (Wire.facts (("host.runtime", runtime ()) :: flags ()));
     Option.iter (fun s -> write t (Wire.snapshot s)) snapshot;
     if required && not (wait_until t ~timeout:ready_timeout (fun () -> t.ready) && Atomic.get t.active) then
       raise (Recorder_error "kavach-recorder did not become ready")
   with e ->
     let msg =
       match e with
       | Recorder_error m -> m
       | Unix.Unix_error (err, _, arg) -> Printf.sprintf "%s: %s" arg (Unix.error_message err)
       | e -> Printexc.to_string e
     in
     if required then (kill t; raise (Recorder_error ("kavach-recorder could not be started: " ^ msg)))
     else (Atomic.set t.active false; log "could not start the recorder (%s); steps run unrecorded" msg));
  at_exit (fun () -> close t);
  t

let recording t = Atomic.get t.active
let file t = locked t (fun () -> t.file)

let answer_snapshot_request t =
  let requested = locked t (fun () -> let r = t.snapshot_requested in t.snapshot_requested <- false; r) in
  if requested && Atomic.get t.active && t.snapshots then
    match t.inst.snapshot with
    | None -> ()
    | Some snap -> (
        match snap () with
        | data -> write t (Wire.snapshot data)
        | exception e -> log "snapshot failed (%s); staying in the current segment" (Printexc.to_string e))

let step t (input : Input.t) =
  if t.closed then invalid_arg "Kavach.Recorder.step: recorder is closed";
  answer_snapshot_request t;
  t.buf <- Buffer.create 256;
  t.outputs <- [];
  (* The input goes in before the handler runs, so a step that kills the
     process still leaves it on record (§10.2). *)
  write t (Wire.input input);
  let failure = t.inst.run (record_env t) input in
  Option.iter
    (fun (f : Handler.failure) -> rec_ t (Wire.marker ~kind:f.kind ~message:f.message ~data:f.detail))
    failure;
  rec_ t Wire.step_end;
  write t (Buffer.contents t.buf);
  let outputs = List.rev t.outputs in
  t.buf <- Buffer.create 0;
  t.outputs <- [];
  match failure with
  | Some f -> Error f
  | None -> if outputs <> [] then t.deliver outputs; Ok ()

let flush ?(durable = false) ?(timeout = 10.) t =
  if not (Atomic.get t.active) then false
  else begin
    write t (Wire.flush ~durable);
    if not durable then Atomic.get t.active
    else begin
      t.flushes <- t.flushes + 1;
      let target = t.flushes in
      (* §10.3: one [durable] per durable flush, in order; pair by counting. *)
      wait_until t ~timeout (fun () -> t.durable >= target || not (Atomic.get t.active))
      && locked t (fun () -> t.durable >= target)
    end
  end
