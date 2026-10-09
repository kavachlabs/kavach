{-# LANGUAGE ForeignFunctionInterface #-}

-- | The SDK half of the shared-memory ring (SPEC §10.7).
--
-- The process library cannot hand a child descriptor 3, so the ring file
-- keeps its name and is passed as the open frame's @ring_path@.
module Kavach.Ring
  ( Ring (..)
  , defaultRingBytes
  , createRing
  , tryPublish
  , tryPublishDirect
  , destroyRing
  , realtimeNanos
  ) where

import Control.Exception (finally, onException)
import Control.Monad (when)
import Data.Bits (popCount)
import qualified Data.ByteString as BS
import Data.ByteString.Unsafe (unsafeUseAsCStringLen)
import Data.IORef
import Data.Time.Clock.POSIX (getPOSIXTime)
import Data.Int (Int64)
import Data.Word (Word64, Word8)
import Foreign.C.Types (CInt (..), CSize (..))
import Foreign.Marshal.Utils (copyBytes)
import Foreign.Ptr
import Foreign.Storable (pokeByteOff)
import System.Directory (doesDirectoryExist, getTemporaryDirectory, removeFile)
import System.IO.Error (catchIOError)
import System.Posix.Files (setFdSize)
import System.Posix.IO
import System.Posix.Process (getProcessID)

foreign import ccall unsafe "kavach_ring_map"
  c_ring_map :: CInt -> CSize -> CSize -> IO (Ptr ())

foreign import ccall unsafe "munmap"
  c_munmap :: Ptr () -> CSize -> IO CInt

foreign import ccall unsafe "kavach_load_acquire"
  loadAcquire :: Ptr Word64 -> IO Word64

foreign import ccall unsafe "kavach_store_release"
  storeRelease :: Ptr Word64 -> Word64 -> IO ()

foreign import ccall unsafe "kavach_now_nanos"
  realtimeNanos :: IO Int64

data Ring = Ring
  { ringMem :: Ptr Word8
  , ringCap :: Int
  , ringPath :: FilePath
  , ringBelled :: IORef Bool
  -- ^ the doorbell rang since the ring was last at most half full
  }

headerBytes, offWrite, offRead :: Int
headerBytes = 65536
offWrite = 64
offRead = 128

defaultRingBytes :: Int
defaultRingBytes = 8 * 1024 * 1024

-- | Create, size, map and initialize the ring file. The caller removes the
-- file ('destroyRing') if the recorder never opens it.
createRing :: Int -> IO Ring
createRing cap = do
  when (cap < 64 * 1024 || popCount cap /= 1) $
    ioError (userError "ring capacity must be a power of two of at least 64 KiB")
  shm <- doesDirectoryExist "/dev/shm"
  dir <- if shm then pure "/dev/shm" else getTemporaryDirectory
  pid <- getProcessID
  t <- getPOSIXTime
  let path = dir ++ "/kavach-ring-" ++ show pid ++ "-" ++ show (floor (t * 1000000) :: Integer)
      size = headerBytes + cap
  fd <- openFd path ReadWrite defaultFileFlags {creat = Just 0o600, exclusive = True}
  ( ( do
      setFdSize fd (fromIntegral size)
      p <- c_ring_map (fromIntegral fd) (fromIntegral headerBytes) (fromIntegral cap)
      if p == nullPtr then ioError (userError "mmap failed") else do
        let mem = castPtr p :: Ptr Word8
        mapM_ (uncurry (pokeByteOff mem)) (zip [0 ..] (BS.unpack "KVRING02"))
        pokeByteOff mem 8 (fromIntegral cap :: Word64)
        belled <- newIORef False
        pure (Ring mem cap path belled)
    )
      `onException` removeQuiet path
    )
    `finally` closeFd fd

-- | Publish bytes with one release store of @write@. Returns how many bytes
-- went in and how many are unread afterwards. Nothing is published if the
-- bytes fit in the ring but not in its free space; a buffer larger than the
-- whole ring publishes as much as fits.
tryPublish :: Ring -> BS.ByteString -> IO (Int, Int)
tryPublish g bs = do
  let wp = castPtr (ringMem g) `plusPtr` offWrite :: Ptr Word64
      rp = castPtr (ringMem g) `plusPtr` offRead :: Ptr Word64
      cap = ringCap g
  w <- loadAcquire wp
  r <- loadAcquire rp
  let used = fromIntegral (w - r) :: Int
      len = BS.length bs
      free = cap - used
      n | len <= free = len
        | len <= cap = 0
        | otherwise = free
  if n == 0 then pure (0, used) else do
    -- the data area is mapped twice, so a wrapping run is one contiguous copy
    let off = fromIntegral (w `mod` fromIntegral cap)
    unsafeUseAsCStringLen bs $ \(src, _) ->
      copyBytes (ringMem g `plusPtr` (headerBytes + off)) (castPtr src) n
    storeRelease wp (w + fromIntegral n)
    pure (n, used + n)

-- | Publish @n@ bytes written by @w@ straight into the ring, when they fit in
-- the free space. Returns the unread bytes afterwards, or
-- Nothing if the caller must use 'tryPublish'.
tryPublishDirect :: Ring -> Int -> (Ptr Word8 -> IO ()) -> IO (Maybe Int)
tryPublishDirect g n w = do
  let wp = castPtr (ringMem g) `plusPtr` offWrite :: Ptr Word64
      rp = castPtr (ringMem g) `plusPtr` offRead :: Ptr Word64
      cap = ringCap g
  wr <- loadAcquire wp
  rd <- loadAcquire rp
  let used = fromIntegral (wr - rd) :: Int
      off = fromIntegral (wr `mod` fromIntegral cap)
  if n > cap - used then pure Nothing else do
    w (ringMem g `plusPtr` (headerBytes + off))
    storeRelease wp (wr + fromIntegral n)
    pure (Just (used + n))

-- | Unmap the ring and remove its file if the recorder has not.
destroyRing :: Ring -> IO ()
destroyRing g = do
  removeQuiet (ringPath g)
  _ <- c_munmap (castPtr (ringMem g)) (fromIntegral (headerBytes + 2 * ringCap g))
  pure ()

removeQuiet :: FilePath -> IO ()
removeQuiet p = removeFile p `catchIOError` \_ -> pure ()
