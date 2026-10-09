-- | Step overhead, the Haskell counterpart of Go's BenchmarkRecorderStep: one
-- clock read, one 8-byte random read and one emit per step, a 33-byte input,
-- 4 journal events per step, a deterministic random source.
--
-- > KAVACH_RECORDER=/path/to/kavach-recorder kavach-bench [steps [pipe|ring]]
--
-- Prints the median ns/event of 5 runs over the pipe and over the ring.
module Main (main) where

import Control.Monad (forM, forM_)
import qualified Data.ByteString as BS
import Data.IORef
import Data.List (sort)
import Data.String (fromString)
import Data.Time.Clock (diffUTCTime, getCurrentTime)
import Kavach
import System.Directory (getTemporaryDirectory, removeDirectoryRecursive)
import System.Environment (getArgs)
import System.IO (hPutStrLn, stderr)

handler :: Handler ()
handler =
  Handler
    { handle = \inp () -> do
        _ <- nowNanos
        _ <- random 8
        emit "entries" (inputData inp)
    , initial = ()
    , snapshot = Just (const "{}")
    , restore = Just (const (Right ()))
    , invariants = []
    }

run :: Bool -> Int -> IO Double
run noRing n = do
  tmp <- getTemporaryDirectory
  let dir = tmp ++ "/kavach-hs-bench"
  ctr <- newIORef (0 :: Int)
  let opts =
        (defaultOptions "bench")
          { roDir = Just (fromString dir)
          , roNoRing = noRing
          , roRequired = True
          , roRandom = \k -> do
              modifyIORef' ctr (+ 1)
              c <- readIORef ctr
              pure (BS.replicate k (fromIntegral c))
          }
      inp = Input "bench" "0" "{\"account\":\"alice\",\"amount\":10,\"x\":1}"
  r <- newRecorder opts handler
  t0 <- getCurrentTime
  forM_ [1 .. n] $ \_ -> step r inp
  t1 <- getCurrentTime
  closeRecorder r
  removeDirectoryRecursive dir
  pure (realToFrac (diffUTCTime t1 t0) * 1e9 / fromIntegral (n * 4))

main :: IO ()
main = do
  args <- getArgs
  let n = case args of a : _ -> read a; _ -> 100000
      only = drop 1 args
  forM_ [t | t@(name, _) <- [("pipe", True), ("ring", False)], null only || name `elem` only] $ \(name, noRing) -> do
    rs <- forM [1 .. 5 :: Int] $ \_ -> run noRing n
    hPutStrLn stderr (name ++ " runs (ns/event): " ++ unwords (map (show . tenth) rs))
    putStrLn (name ++ " median ns/event: " ++ show (tenth (sort rs !! 2)))
  where
    tenth x = fromIntegral (round (x * 10) :: Int) / 10 :: Double
