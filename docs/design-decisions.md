# foundationdb vs pebble
ended up going with foundation

- fdb
  - distributed systems built in
  - hierarchical keys built in
    - MUCH easier to use
  - blob limit: 100kb
- pebble
  - easier integration with golang; literally just a package
  - blob limit = 4gb
