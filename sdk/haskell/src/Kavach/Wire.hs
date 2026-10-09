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
  , Enc (..)
  , encBytes
  , encRaw
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
import qualified Data.ByteString.Internal as BI
import qualified Data.ByteString.Unsafe as BU
import Data.Int (Int64)
import Data.Text (Text)
import qualified Data.Text.Foreign as TF
import Data.Word (Word64, Word8)
import Foreign.Marshal.Utils (copyBytes)
import Foreign.Ptr (Ptr, castPtr, plusPtr)
import Foreign.Storable (poke)

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

-- | A piece of the record stream whose size is known before it is written, so
-- a step's frames are written into one buffer without intermediate copies.
data Enc = Enc !Int (Ptr Word8 -> IO ())

instance Semigroup Enc where
  Enc a f <> Enc b g = Enc (a + b) (\p -> f p >> g (p `plusPtr` a))

instance Monoid Enc where
  mempty = Enc 0 (\_ -> pure ())

encBytes :: Enc -> ByteString
encBytes (Enc n w) = BI.unsafeCreate n w

encRaw :: ByteString -> Enc
encRaw b = Enc (BS.length b) (\p -> BU.unsafeUseAsCStringLen b (\(src, n) -> copyBytes p (castPtr src) n))

byteE :: Word8 -> Enc
byteE b = Enc 1 (\p -> poke p b)

uvarint :: Int -> Enc
uvarint n = Enc (len n) (\p -> go p n)
  where
    len k = if k >= 0x80 then 1 + len (k `shiftR` 7) else 1 :: Int
    go p k
      | k >= 0x80 = poke p (fromIntegral (k .&. 0x7f) .|. 0x80) >> go (p `plusPtr` 1) (k `shiftR` 7)
      | otherwise = poke p (fromIntegral k)

bytesF :: ByteString -> Enc
bytesF b = uvarint (BS.length b) <> encRaw b

stringF :: Text -> Enc
stringF t = let n = TF.lengthWord8 t in uvarint n <> Enc n (TF.unsafeCopyToPtr t)

scopeF :: Bool -> Enc
scopeF local = byteE (if local then 1 else 0)

frame :: Word8 -> Enc -> Enc
frame kind payload@(Enc n _) = uvarint (1 + n) <> byteE kind <> payload

frameOpen :: ByteString -> ByteString
frameOpen = encBytes . frame 0x01 . encRaw

frameStepEnd, frameClose :: ByteString
frameStepEnd = encBytes (frame 0x03 mempty)
frameClose = encBytes (frame 0x07 mempty)

-- | A @facts@ frame whose facts all have form 0 (value).
frameFacts :: [(Text, ByteString)] -> ByteString
frameFacts kvs =
  encBytes (frame 0x04 (uvarint (length kvs) <> mconcat [stringF k <> byteE 0 <> bytesF v | (k, v) <- kvs]))

frameSnapshot :: ByteString -> ByteString
frameSnapshot = encBytes . frame 0x05 . bytesF

frameFlush :: Bool -> ByteString
frameFlush durable = encBytes (frame 0x06 (byteE (if durable then 1 else 0)))

record :: Word8 -> Bool -> Enc -> Enc
record ty critical payload = frame 0x02 (byteE ty <> byteE (if critical then 1 else 0) <> payload)

recInput :: Text -> Text -> ByteString -> Enc
recInput src pos d = record 0x01 False (stringF src <> stringF pos <> bytesF d)

recClock :: Int64 -> Enc
recClock ns = record 0x02 False (Enc 8 (\p -> poke (castPtr p) (fromIntegral ns :: Word64)))

recRand :: ByteString -> Enc
recRand = record 0x03 False . bytesF

recOutput :: Text -> ByteString -> Bool -> Enc
recOutput sink d local = record 0x04 False (stringF sink <> bytesF d <> scopeF local)

recMarker :: Text -> Text -> ByteString -> Enc
recMarker kind msg d = record 0x05 False (stringF kind <> stringF msg <> bytesF d)

-- | gateway name, request, response, error, local
recGateway :: Text -> ByteString -> ByteString -> Text -> Bool -> Enc
recGateway g req resp err local =
  record 0x07 True (stringF g <> bytesF req <> bytesF resp <> stringF err <> scopeF local)

-- | key, value if present, source
recConfig :: Text -> Maybe ByteString -> Text -> Enc
recConfig k v src =
  record 0x09 True
    (stringF k <> byteE (maybe 0 (const 1) v) <> bytesF (maybe BS.empty id v) <> stringF src)
