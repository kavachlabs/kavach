(* Runs the SDK recorder cases of SPEC §10.6 through the fake recorder.

     recorder_cases.exe [case.json ...]

   With no arguments, every case in $KAVACH_SPEC_DIR/recorder/sdk. *)
open Kavach

let spec_dir =
  match Sys.getenv_opt "KAVACH_SPEC_DIR" with
  | Some d when d <> "" -> d
  | _ -> "/Users/koustav/code/kavach-labs/kavach/spec"

let read_file p = In_channel.with_open_bin p In_channel.input_all
let get k j = match Json.member k j with Some v -> v | None -> failwith ("case: missing " ^ k)
let str k j = Json.to_str (get k j)
let list k j = match Json.member k j with Some (Json.List l) -> l | _ -> []

let run_case fake path =
  let case = Json.parse (read_file path) in
  let result = Filename.temp_file "kavach-case" ".json" in
  let op = get "open" case in
  let snapshot = match Json.member "snapshot" case with Some (Json.String s) -> Some s | _ -> None in
  let flags =
    match Json.member "flags" case with
    | Some (Json.Obj l) -> List.map (fun (k, v) -> (k, Json.to_str v)) l
    | _ -> []
  in
  let answers = Hashtbl.create 4 in
  let next kind =
    match Hashtbl.find_opt answers kind with
    | Some (x :: rest) -> Hashtbl.replace answers kind rest; x
    | _ -> failwith ("case: no " ^ kind ^ " answer left")
  in
  let result_of_answer a =
    match Json.member "error" a with
    | Some (Json.String e) -> Error e
    | _ -> Ok (Base64.decode (str "response" a))
  in
  let rec_ =
    Recorder.create
      ~recorder_command:[| "python3"; fake; path; result |]
      ~service:(str "service" op) ?snapshot
      ~snapshots:(Json.member "snapshots" op = Some (Json.Bool true))
      ~gateways:(fun _ -> Some { Recorder.local = false; call = (fun _ -> result_of_answer (next "gateway")) })
      ~config:(fun _ ->
        let a = next "config" in
        match Json.member "value" a with Some (Json.String v) -> Some (Base64.decode v) | _ -> None)
      ~flags:(fun () -> List.map (fun (k, v) -> (k, v)) flags)
      ~clock:(fun () -> Int64.of_string (Json.to_str (next "clock")))
      ~random:(fun n ->
        let d = Base64.decode (Json.to_str (next "rand")) in
        if String.length d <> n then failwith "case: rand answer has the wrong length";
        d)
      Conformance.handler
  in
  List.iter
    (fun action ->
      match Json.member "step" action with
      | Some step ->
          Hashtbl.reset answers;
          (match Json.member "answers" action with
          | Some (Json.Obj kinds) -> List.iter (fun (k, v) -> match v with Json.List l -> Hashtbl.replace answers k l | _ -> ()) kinds
          | _ -> ());
          let input : Input.t =
            { source = str "source" step; position = str "position" step; data = Base64.decode (str "data" step) }
          in
          ignore (Recorder.step rec_ input)
      | None ->
          if not (Recorder.flush ~durable:true rec_) then failwith "case: flush was not answered")
    (list "actions" case);
  Recorder.close rec_;
  let r = Json.parse (read_file result) in
  Sys.remove result;
  match Json.member "pass" r with
  | Some (Json.Bool true) -> None
  | _ -> Some (match Json.member "error" r with Some (Json.String e) -> e | _ -> "no result")

let () =
  let cases =
    match List.tl (Array.to_list Sys.argv) with
    | [] ->
        let dir = Filename.concat spec_dir "recorder/sdk" in
        Sys.readdir dir |> Array.to_list |> List.sort compare
        |> List.filter (fun f -> Filename.check_suffix f ".json")
        |> List.map (Filename.concat dir)
    | l -> l
  in
  let fake = Filename.concat spec_dir "recorder/sdk/fake_recorder.py" in
  let failed = ref 0 in
  List.iter
    (fun path ->
      match run_case fake path with
      | None -> Printf.printf "PASS  %s\n%!" (Filename.basename path)
      | Some e -> incr failed; Printf.printf "FAIL  %s\n  %s\n%!" (Filename.basename path) e
      | exception e -> incr failed; Printf.printf "FAIL  %s\n  %s\n%!" (Filename.basename path) (Printexc.to_string e))
    cases;
  Printf.printf "%d/%d passed\n" (List.length cases - !failed) (List.length cases);
  exit (if !failed = 0 then 0 else 1)
