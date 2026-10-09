(* The host side of SPEC §9: runs a handler one step at a time for the driver.
   Sandbox mode (§6.3) is not supported. *)

exception Aborted

let sdk = Recorder.producer

let str k j = match Json.member k j with Some (Json.String s) -> s | _ -> ""

(* `kavach-recorder facts` prints the env. and host. facts in the form
   ready.environment wants (§9.2). *)
let recorder_facts () =
  let prog = match Sys.getenv_opt "KAVACH_RECORDER" with Some p when p <> "" -> p | _ -> "kavach-recorder" in
  try
    let null = Unix.openfile "/dev/null" [ Unix.O_RDWR ] 0 in
    let r, w = Unix.pipe ~cloexec:true () in
    let pid = Unix.create_process prog [| prog; "facts" |] null w null in
    Unix.close w;
    Unix.close null;
    let ic = Unix.in_channel_of_descr r in
    let out = In_channel.input_all ic in
    close_in ic;
    let _, status = Unix.waitpid [] pid in
    if status <> Unix.WEXITED 0 then []
    else match Json.parse (String.trim out) with Json.Obj l -> l | _ -> []
  with _ -> []

let environment () =
  let value s = Json.Obj [ ("value", Json.String (Base64.encode s)) ] in
  Json.Obj (List.remove_assoc "host.runtime" (recorder_facts ()) @ [ ("host.runtime", value (Recorder.runtime ())) ])

let run (module H : Handler.S) =
  if Sys.os_type = "Unix" then Sys.set_signal Sys.sigpipe Sys.Signal_ignore;
  (* Take the protocol stream for ourselves; whatever the handler prints goes
     to stderr (§9.1). *)
  let oc = Unix.out_channel_of_descr (Unix.dup Unix.stdout) in
  flush stdout;
  Unix.dup2 Unix.stderr Unix.stdout;
  let ic = stdin in
  let send j =
    try
      output_string oc (Json.to_string j);
      output_char oc '\n';
      flush oc
    with Sys_error _ -> exit 1
  in
  let obj t fields = Json.Obj (("t", Json.String t) :: fields) in
  let fatal m = send (obj "fatal" [ ("message", Json.String m) ]); exit 1 in
  let read () =
    match input_line ic with
    | exception End_of_file -> None
    | line -> (
        match Json.parse line with
        | Json.Obj _ as j -> Some j
        | _ | (exception Json.Parse_error _) -> fatal "malformed message")
  in
  let bytes_of k j = try Base64.decode (str k j) with Invalid_argument _ -> fatal ("bad base64 in " ^ k) in
  let scope local = Json.String (if local then "local" else "remote") in
  let inst = ref None in
  let do_step inst msg =
    let aborted = ref false in
    let request t fields wanted =
      if !aborted then raise Aborted;
      send (obj t fields);
      match read () with
      | None -> exit 1
      | Some reply ->
          let rt = str "t" reply in
          if rt = "abort" then (aborted := true; raise Aborted)
          else if rt = wanted then reply
          else fatal (Printf.sprintf "unexpected %s message while waiting for %s" rt wanted)
    in
    let env : Env.t =
      {
        clock = (fun () -> Int64.of_string (str "unix_nanos" (request "clock" [] "clock")));
        rand =
          (fun n ->
            let d = bytes_of "data" (request "rand" [ ("n", Json.Int n) ] "rand") in
            if String.length d <> n then fatal "rand answer has the wrong length";
            d);
        query =
          (fun ~gateway request_bytes ->
            let r =
              request "gateway"
                [ ("gateway", Json.String gateway); ("request", Json.String (Base64.encode request_bytes));
                  ("scope", scope false) ]
                "gateway"
            in
            match Json.member "error" r with
            | Some (Json.String e) -> Error e
            | _ -> if Json.member "live" r <> None then Error "live queries are not supported" else Ok (bytes_of "response" r));
        config =
          (fun key ->
            let r = request "config" [ ("key", Json.String key) ] "config" in
            if Json.member "present" r = Some (Json.Bool true) then Some (bytes_of "value" r) else None);
        emit =
          (fun ~local ~sink data ->
            if !aborted then raise Aborted;
            send (obj "emit" [ ("sink", Json.String sink); ("data", Json.String (Base64.encode data)); ("scope", scope local) ]));
      }
    in
    let input : Input.t = { source = str "source" msg; position = str "position" msg; data = bytes_of "data" msg } in
    let failure = (inst : Handler.instance).run env input in
    let outcome, fields =
      if !aborted then ("aborted", [])
      else
        match failure with
        | None -> ("ok", [])
        | Some f ->
            ( f.kind,
              ("message", Json.String f.message) :: (if f.detail = "" then [] else [ ("detail", Json.String f.detail) ]) )
    in
    send (obj "done" (("outcome", Json.String outcome) :: fields))
  in
  let rec loop () =
    match read () with
    | None -> ()
    | Some msg -> (
        match str "t" msg with
        | "hello" ->
            if Json.member "protocol" msg <> Some (Json.Int 1) then fatal "unsupported protocol version";
            if str "mode" msg = "sandbox" then fatal "sandbox mode not supported";
            let snapshot =
              if str "start" msg = "snapshot" then Some (bytes_of "snapshot" msg) else None
            in
            (match Handler.instantiate (module H) ~snapshot with
            | Error m -> fatal m
            | Ok i ->
                inst := Some i;
                send
                  (obj "ready"
                     [ ("protocol", Json.Int 1); ("sdk", Json.String sdk);
                       ("invariants", Json.List (List.map (fun n -> Json.String n) i.invariants));
                       ("environment", environment ()) ]));
            loop ()
        | "step" -> (
            match !inst with
            | None -> fatal "step before hello"
            | Some i -> do_step i msg; loop ())
        | "end" -> ()
        | t -> fatal ("unexpected message " ^ t))
  in
  loop ()

let is_host () =
  let n = Array.length Sys.argv in
  n > 1 && Sys.argv.(n - 1) = "kavach-host"

let maybe_host h = if is_host () then (run h; exit 0)
