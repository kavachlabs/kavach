-- | The conformance host (SPEC §9.6): run with @kavach-host@ as its last argument.
module Main (main) where

import Kavach (maybeHost)
import Kavach.Conformance (conformance)

main :: IO ()
main = maybeHost conformance >> putStrLn "usage: kavach-conformance-host kavach-host"
