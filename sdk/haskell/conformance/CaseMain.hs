-- | Runs one SDK recorder case (spec/recorder/sdk/README.md) through this SDK.
module Main (main) where

import RecorderCase (runCase, specDir)
import System.Environment (getArgs)
import System.Exit (exitFailure, exitSuccess)
import System.IO

main :: IO ()
main = do
  args <- getArgs
  case args of
    [path] -> do
      dir <- specDir
      r <- runCase dir path
      case r of
        Nothing -> putStrLn ("PASS  " ++ path) >> exitSuccess
        Just err -> putStrLn ("FAIL  " ++ path) >> hPutStrLn stderr err >> exitFailure
    _ -> hPutStrLn stderr "usage: kavach-recorder-case <case.json>" >> exitFailure
