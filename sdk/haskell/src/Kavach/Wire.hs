-- | Encoding of the record stream (SPEC §10.2), its record payloads (§4) and
-- base64 (RFC 4648 §4, padded) for the host protocol.
module Kavach.Wire
  ( base64Encode
  , base64Decode
  , frameOpen
  , frameStepEnd
  , frameFacts
  , frameSnapshot
  , frameFlush
  , frameClose
  , recInput
  , recClock
  , recRand
  , recOutput
  , recMarker
  , recGateway
  , recConfig
  ) where

import Data.Bits (shiftL, shiftR, (.&.), (.|.))
import qualified Data.ByteString as BS
import Data.ByteString (ByteString)
import qualified Data.ByteString.Builder as B
import qualified Data.ByteString.Lazy as BL
import qualified Data.ByteString.Unsafe as BU
import Data.Int (Int64)
import Data.Text (Text)
import qualified Data.Text.Encoding as TE
import Data.Word (Word8)

build :: B.Builder -> ByteString
build = BL.toStrict . B.toLazyByteString

alphabet :: ByteString
alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

base64Encode :: ByteString -> ByteString
base64Encode = build . go
  where
    ch n = B.word8 (BU.unsafeIndex alphabet (fromIntegral (n .&. 63)))
    go bs = case BS.unpack (BS.take 3 bs) of
      [] -> mempty
      [a, b, c] -> quad a b c 3 <> go (BS.drop 3 bs)
      [a, b] -> quad a b 0 2
      [a] -> quad a 0 0 1
      _ -> mempty
    quad :: Word8 -> Word8 -> Word8 -> Int -> B.Builder
    quad a b c n =
      let w = (fromIntegral a `shiftL` 16 .|. fromIntegral b `shiftL` 8 .|. fromIntegral c) :: Int
       in ch (w `shiftR` 18) <> ch (w `shiftR` 12)
            <> (if n >= 2 then ch (w `shiftR` 6) else B.char7 '=')
            <> (if n >= 3 then ch w else B.char7 '=')

-- | Strict decoding: padded, no stray characters.
base64Decode :: ByteString -> Maybe ByteString
base64Decode s
  | BS.length s `mod` 4 /= 0 = Nothing
  | otherwise = build . mconcat <$> mapM quad (zip [1 :: Int ..] (chunks s))
  where
    n4 = BS.length s `div` 4
    chunks b | BS.null b = [] | otherwise = BS.take 4 b : chunks (BS.drop 4 b)
    val c = fromIntegral <$> BS.elemIndex c alphabet :: Maybe Int
    quad (i, q) = do
      let pad = BS.length (BS.takeWhileEnd (== 61) q)
      if pad > 2 || (pad > 0 && i /= n4) then Nothing else Just ()
      vs <- mapM val (BS.unpack (BS.take (4 - pad) q))
      let w = foldl (\a v -> a `shiftL` 6 .|. v) 0 (vs ++ replicate pad 0)
          bytes = [fromIntegral (w `shiftR` 16), fromIntegral (w `shiftR` 8), fromIntegral w] :: [Word8]
      Just (mconcat (map B.word8 (take (3 - pad) bytes)))

uvarint :: Int -> B.Builder
uvarint n
  | n >= 0x80 = B.word8 (fromIntegral (n .&. 0x7f) .|. 0x80) <> uvarint (n `shiftR` 7)
  | otherwise = B.word8 (fromIntegral n)

bytesF :: ByteString -> B.Builder
bytesF b = uvarint (BS.length b) <> B.byteString b

stringF :: Text -> B.Builder
stringF = bytesF . TE.encodeUtf8

scopeF :: Bool -> B.Builder
scopeF local = B.word8 (if local then 1 else 0)

frame :: Word8 -> B.Builder -> ByteString
frame kind payload =
  let p = build payload in build (uvarint (1 + BS.length p) <> B.word8 kind <> B.byteString p)

frameOpen :: ByteString -> ByteString
frameOpen = frame 0x01 . B.byteString

frameStepEnd, frameClose :: ByteString
frameStepEnd = frame 0x03 mempty
frameClose = frame 0x07 mempty

-- | A @facts@ frame whose facts all have form 0 (value).
frameFacts :: [(Text, ByteString)] -> ByteString
frameFacts kvs =
  frame 0x04 (uvarint (length kvs) <> mconcat [stringF k <> B.word8 0 <> bytesF v | (k, v) <- kvs])

frameSnapshot :: ByteString -> ByteString
frameSnapshot = frame 0x05 . bytesF

frameFlush :: Bool -> ByteString
frameFlush durable = frame 0x06 (B.word8 (if durable then 1 else 0))

record :: Word8 -> Bool -> B.Builder -> ByteString
record ty critical payload = frame 0x02 (B.word8 ty <> B.word8 (if critical then 1 else 0) <> payload)

recInput :: Text -> Text -> ByteString -> ByteString
recInput src pos d = record 0x01 False (stringF src <> stringF pos <> bytesF d)

recClock :: Int64 -> ByteString
recClock = record 0x02 False . B.int64LE

recRand :: ByteString -> ByteString
recRand = record 0x03 False . bytesF

recOutput :: Text -> ByteString -> Bool -> ByteString
recOutput sink d local = record 0x04 False (stringF sink <> bytesF d <> scopeF local)

recMarker :: Text -> Text -> ByteString -> ByteString
recMarker kind msg d = record 0x05 False (stringF kind <> stringF msg <> bytesF d)

-- | gateway name, request, response, error, local
recGateway :: Text -> ByteString -> ByteString -> Text -> Bool -> ByteString
recGateway g req resp err local =
  record 0x07 True (stringF g <> bytesF req <> bytesF resp <> stringF err <> scopeF local)

-- | key, value if present, source
recConfig :: Text -> Maybe ByteString -> Text -> ByteString
recConfig k v src =
  record 0x09 True
    (stringF k <> B.word8 (maybe 0 (const 1) v) <> bytesF (maybe BS.empty id v) <> stringF src)
