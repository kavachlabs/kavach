{-# LANGUAGE NoOverloadedStrings #-}
-- | A small JSON reader and writer, enough for the host and recorder
-- protocols (SPEC §9.2, §10.3). Objects keep their key order.
module Kavach.Json
  ( Json (..)
  , parseJson
  , renderJson
  , field
  , asText
  , asInt
  , asBool
  ) where

import Data.Bits (shiftL, (.|.))
import Data.ByteString (ByteString)
import qualified Data.ByteString.Builder as B
import qualified Data.ByteString.Lazy as BL
import Data.Char (chr, digitToInt, isDigit, isHexDigit, ord)
import Data.List (intersperse)
import Data.Text (Text)
import qualified Data.Text as T
import qualified Data.Text.Encoding as TE

data Json
  = JNull
  | JBool Bool
  | JNum String -- ^ the number as written
  | JStr Text
  | JArr [Json]
  | JObj [(Text, Json)]
  deriving (Eq, Show)

field :: Text -> Json -> Maybe Json
field k (JObj kvs) = lookup k kvs
field _ _ = Nothing

asText :: Json -> Maybe Text
asText (JStr t) = Just t
asText _ = Nothing

asInt :: Json -> Maybe Integer
asInt (JNum s)
  | not (null digits) && all isDigit digits = Just (read s)
  where
    digits = case s of
      '-' : r -> r
      r -> r
asInt _ = Nothing

asBool :: Json -> Maybe Bool
asBool (JBool b) = Just b
asBool _ = Nothing

-- | Parse one JSON value from UTF-8 bytes; trailing whitespace is allowed.
parseJson :: ByteString -> Either String Json
parseJson bs = do
  t <- either (Left . show) Right (TE.decodeUtf8' bs)
  (v, rest) <- value (T.unpack t)
  if all ws rest then Right v else Left "trailing characters after JSON value"

ws :: Char -> Bool
ws c = c `elem` " \t\r\n"

type P a = String -> Either String (a, String)

value :: P Json
value s = case dropWhile ws s of
  '{' : r -> object (dropWhile ws r) []
  '[' : r -> array (dropWhile ws r) []
  '"' : r -> do (t, r') <- str r; Right (JStr (T.pack t), r')
  't' : 'r' : 'u' : 'e' : r -> Right (JBool True, r)
  'f' : 'a' : 'l' : 's' : 'e' : r -> Right (JBool False, r)
  'n' : 'u' : 'l' : 'l' : r -> Right (JNull, r)
  r@(c : _)
    | c == '-' || isDigit c ->
        let (n, r') = span (`elem` "+-.eE0123456789") r in Right (JNum n, r')
  _ -> Left "unexpected character in JSON"

object :: String -> [(Text, Json)] -> Either String (Json, String)
object ('}' : r) [] = Right (JObj [], r)
object ('"' : r) acc = do
  (k, r1) <- str r
  case dropWhile ws r1 of
    ':' : r2 -> do
      (v, r3) <- value r2
      case dropWhile ws r3 of
        ',' : r4 -> object (dropWhile ws r4) ((T.pack k, v) : acc)
        '}' : r4 -> Right (JObj (reverse ((T.pack k, v) : acc)), r4)
        _ -> Left "expected , or } in object"
    _ -> Left "expected : in object"
object _ _ = Left "expected a string key in object"

array :: String -> [Json] -> Either String (Json, String)
array (']' : r) [] = Right (JArr [], r)
array s acc = do
  (v, r1) <- value s
  case dropWhile ws r1 of
    ',' : r2 -> array (dropWhile ws r2) (v : acc)
    ']' : r2 -> Right (JArr (reverse (v : acc)), r2)
    _ -> Left "expected , or ] in array"

str :: P String
str = go []
  where
    go acc ('"' : r) = Right (reverse acc, r)
    go acc ('\\' : c : r) = case c of
      '"' -> go ('"' : acc) r
      '\\' -> go ('\\' : acc) r
      '/' -> go ('/' : acc) r
      'b' -> go ('\b' : acc) r
      'f' -> go ('\f' : acc) r
      'n' -> go ('\n' : acc) r
      'r' -> go ('\r' : acc) r
      't' -> go ('\t' : acc) r
      'u' -> do
        (u, r1) <- hex4 r
        if u >= 0xD800 && u < 0xDC00
          then case r1 of
            '\\' : 'u' : r2 -> do
              (lo, r3) <- hex4 r2
              if lo >= 0xDC00 && lo < 0xE000
                then go (chr (0x10000 + (((u - 0xD800) `shiftL` 10) .|. (lo - 0xDC00))) : acc) r3
                else Left "bad surrogate pair"
            _ -> Left "lone surrogate"
          else
            if u >= 0xDC00 && u < 0xE000
              then Left "lone surrogate"
              else go (chr u : acc) r1
      _ -> Left "bad escape"
    go acc (c : r) | c >= ' ' = go (c : acc) r
    go _ _ = Left "unterminated or invalid string"
    hex4 r = case splitAt 4 r of
      (h, r') | length h == 4 && all isHexDigit h -> Right (foldl (\a d -> a * 16 + digitToInt d) 0 h, r')
      _ -> Left "bad \\u escape"

-- | Compact, ASCII-only output, so protocol lines never depend on a locale.
renderJson :: Json -> ByteString
renderJson = BL.toStrict . B.toLazyByteString . go
  where
    go JNull = B.string7 "null"
    go (JBool b) = B.string7 (if b then "true" else "false")
    go (JNum n) = B.string7 n
    go (JStr t) = qstr t
    go (JArr xs) = B.char7 '[' <> mconcat (intersperse (B.char7 ',') (map go xs)) <> B.char7 ']'
    go (JObj kvs) =
      B.char7 '{' <> mconcat (intersperse (B.char7 ',') [qstr k <> B.char7 ':' <> go v | (k, v) <- kvs]) <> B.char7 '}'
    qstr t = B.char7 '"' <> mconcat (map esc (T.unpack t)) <> B.char7 '"'
    esc '"' = B.string7 "\\\""
    esc '\\' = B.string7 "\\\\"
    esc '\n' = B.string7 "\\n"
    esc '\r' = B.string7 "\\r"
    esc '\t' = B.string7 "\\t"
    esc c
      | c >= ' ' && c < '\DEL' = B.char7 c
      | ord c < 0x10000 = u (ord c)
      | otherwise =
          let n = ord c - 0x10000 in u (0xD800 + n `div` 0x400) <> u (0xDC00 + n `mod` 0x400)
    u n = B.string7 "\\u" <> B.word16HexFixed (fromIntegral n)
