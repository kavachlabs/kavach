{-# LANGUAGE BangPatterns #-}

-- | Test helper: records one step that completes and one that SIGKILLs the
-- process once its input is on record. Usage: kavach-kill-helper <dir> [--pipe]
module Main (main) where

import Data.String (fromString)
import Kavach
import System.Environment (getArgs)
import System.IO.Unsafe (unsafePerformIO)
import System.Posix.Process (getProcessID)
import System.Posix.Signals (sigKILL, signalProcess)

handler :: Handler ()
handler =
  Handler
    { handle = \inp () -> do
        _ <- nowNanos
        let !_ = if inputData inp == "die" then unsafePerformIO (getProcessID >>= signalProcess sigKILL) else ()
        pure ()
    , initial = ()
    , snapshot = Nothing
    , restore = Nothing
    , invariants = []
    }

main :: IO ()
main = do
  args <- getArgs
  let opts = (defaultOptions "killed") {roDir = Just (fromString (concat (take 1 args))), roNoRing = "--pipe" `elem` args}
  r <- newRecorder opts handler
  _ <- step r (Input "t" "0" "fine")
  _ <- step r (Input "t" "1" "die")
  closeRecorder r
