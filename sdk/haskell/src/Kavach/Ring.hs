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
  , destroyRing
  ) where

import Control.Exception (finally, onException)
import Control.Monad (when)
import Data.Bits (popCount)
import qualified Data.ByteString as BS
import Data.ByteString.Unsafe (unsafeUseAsCStringLen)
import Data.IORef
import Data.Time.Clock.POSIX (getPOSIXTime)
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

foreign import ccall unsafe "mmap"
  c_mmap :: Ptr () -> CSize -> CInt -> CInt -> CInt -> Int -> IO (Ptr ())

foreign import ccall unsafe "munmap"
  c_munmap :: Ptr () -> CSize -> IO CInt

foreign import ccall unsafe "kavach_load_acquire"
  loadAcquire :: Ptr Word64 -> IO Word64

foreign import ccall unsafe "kavach_store_release"
  storeRelease :: Ptr Word64 -> Word64 -> IO ()

data Ring = Ring
  { ringMem :: Ptr Word8
  , ringCap :: Int
  , ringPath :: FilePath
  , ringBelled :: IORef Bool
  -- ^ the doorbell rang since the ring was last at most half full
  }

headerBytes, offWrite, offRead :: Int
headerBytes = 256
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
      -- PROT_READ|PROT_WRITE = 3, MAP_SHARED = 1 on every unix
      p <- c_mmap nullPtr (fromIntegral size) 3 1 (fromIntegral fd) 0
      if p == nullPtr `plusPtr` (-1) then ioError (userError "mmap failed") else do
        let mem = castPtr p :: Ptr Word8
        mapM_ (uncurry (pokeByteOff mem)) (zip [0 ..] (BS.unpack "KVRING01"))
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
    let off = fromIntegral (w `mod` fromIntegral cap)
        first = min n (cap - off)
        dat = ringMem g `plusPtr` headerBytes
    unsafeUseAsCStringLen bs $ \(src, _) -> do
      copyBytes (dat `plusPtr` off) (castPtr src) first
      copyBytes dat (castPtr src `plusPtr` first) (n - first)
    storeRelease wp (w + fromIntegral n)
    pure (n, used + n)

-- | Unmap the ring and remove its file if the recorder has not.
destroyRing :: Ring -> IO ()
destroyRing g = do
  removeQuiet (ringPath g)
  _ <- c_munmap (castPtr (ringMem g)) (fromIntegral (headerBytes + ringCap g))
  pure ()

removeQuiet :: FilePath -> IO ()
removeQuiet p = removeFile p `catchIOError` \_ -> pure ()
