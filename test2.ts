import { sleep } from "https://deno.land/x/sleep/mod.ts";

let i = 0
while(i < 100) {
    console.log(i)
    sleep(5000)
}