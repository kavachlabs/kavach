(* Encoding of the record stream (SPEC §10.2) and its record payloads (§4). *)

let uvarint b n =
  let n = ref n in
  while !n >= 0x80 do
    Buffer.add_char b (Char.chr (!n land 0x7f lor 0x80));
    n := !n lsr 7
  done;
  Buffer.add_char b (Char.chr !n)

let bytes_field b s = uvarint b (String.length s); Buffer.add_string b s
let u8 b n = Buffer.add_char b (Char.chr n)
let scope b local = u8 b (if local then 1 else 0)

let frame kind fill =
  let p = Buffer.create 64 in
  fill p;
  let out = Buffer.create (Buffer.length p + 6) in
  uvarint out (1 + Buffer.length p);
  u8 out kind;
  Buffer.add_buffer out p;
  Buffer.contents out

let record rtype ~critical fill =
  frame 0x02 (fun p -> u8 p rtype; u8 p (if critical then 1 else 0); fill p)

let input (i : Input.t) =
  record 0x01 ~critical:false (fun p -> bytes_field p i.source; bytes_field p i.position; bytes_field p i.data)

let clock ns = record 0x02 ~critical:false (fun p -> Buffer.add_int64_le p ns)
let rand data = record 0x03 ~critical:false (fun p -> bytes_field p data)

let output ~sink ~local data =
  record 0x04 ~critical:false (fun p -> bytes_field p sink; bytes_field p data; scope p local)

let marker ~kind ~message ~data =
  record 0x05 ~critical:false (fun p -> bytes_field p kind; bytes_field p message; bytes_field p data)

let gateway ~name ~request ~response ~error ~local =
  record 0x07 ~critical:true (fun p ->
      bytes_field p name; bytes_field p request; bytes_field p response; bytes_field p error; scope p local)

let config ~key ~value ~source =
  record 0x09 ~critical:true (fun p ->
      bytes_field p key;
      u8 p (if value = None then 0 else 1);
      bytes_field p (Option.value value ~default:"");
      bytes_field p source)

let open_ json = frame 0x01 (fun p -> Buffer.add_string p json)
let step_end = frame 0x03 ignore

let facts l =
  frame 0x04 (fun p ->
      uvarint p (List.length l);
      List.iter (fun (k, v) -> bytes_field p k; u8 p 0; bytes_field p v) l)

let snapshot data = frame 0x05 (fun p -> bytes_field p data)
let flush ~durable = frame 0x06 (fun p -> u8 p (if durable then 1 else 0))
let close = frame 0x07 ignore
