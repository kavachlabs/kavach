let alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

let encode s =
  let n = String.length s in
  let b = Buffer.create (((n + 2) / 3) * 4) in
  let byte i = if i < n then Char.code s.[i] else 0 in
  let i = ref 0 in
  while !i < n do
    let v = (byte !i lsl 16) lor (byte (!i + 1) lsl 8) lor byte (!i + 2) in
    Buffer.add_char b alphabet.[(v lsr 18) land 63];
    Buffer.add_char b alphabet.[(v lsr 12) land 63];
    Buffer.add_char b (if !i + 1 < n then alphabet.[(v lsr 6) land 63] else '=');
    Buffer.add_char b (if !i + 2 < n then alphabet.[v land 63] else '=');
    i := !i + 3
  done;
  Buffer.contents b

let decode s =
  let n = String.length s in
  if n mod 4 <> 0 then invalid_arg "Base64.decode: length is not a multiple of 4";
  let b = Buffer.create (n / 4 * 3) in
  let sym c =
    match String.index_opt alphabet c with Some v -> v | None -> invalid_arg "Base64.decode: bad character"
  in
  let i = ref 0 in
  while !i < n do
    let pad = if s.[!i + 3] <> '=' then 0 else if s.[!i + 2] <> '=' then 1 else 2 in
    if pad > 0 && !i + 4 < n then invalid_arg "Base64.decode: padding before the end";
    let v =
      (sym s.[!i] lsl 18) lor (sym s.[!i + 1] lsl 12)
      lor ((if pad = 2 then 0 else sym s.[!i + 2]) lsl 6)
      lor if pad >= 1 then 0 else sym s.[!i + 3]
    in
    Buffer.add_char b (Char.chr ((v lsr 16) land 255));
    if pad < 2 then Buffer.add_char b (Char.chr ((v lsr 8) land 255));
    if pad < 1 then Buffer.add_char b (Char.chr (v land 255));
    i := !i + 4
  done;
  Buffer.contents b
